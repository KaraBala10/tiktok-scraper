package profile

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var secUIDRE = regexp.MustCompile(`"secUid":"(MS4wLjABAAAA[^"]+)"`)

type timings struct {
	DNS, Connect, TLS, TTFB, Download, Parse, Total time.Duration
	Resumed                                         bool
	Pages                                           int
}

func (t timings) String() string {
	wait := t.TTFB - t.DNS - t.Connect - t.TLS
	if wait < 0 {
		wait = 0
	}
	total := t.Total
	if total == 0 {
		total = t.TTFB + t.Download + t.Parse
	}
	return fmt.Sprintf(
		"dns=%s connect=%s tls=%s resume=%t wait=%s download=%s parse=%s pages=%d total=%s",
		t.DNS.Round(time.Millisecond),
		t.Connect.Round(time.Millisecond),
		t.TLS.Round(time.Millisecond),
		t.Resumed,
		wait.Round(time.Millisecond),
		t.Download.Round(time.Millisecond),
		t.Parse.Round(time.Millisecond),
		t.Pages,
		total.Round(time.Millisecond),
	)
}
func fetchVideoURLs(username string, limit int) (int, timings, error) {
	start := time.Now()
	s := newStream(limit)
	defer s.finish()
	paginate := limit <= 0 || limit > 10

	if paginate {
		if uid, ok := loadCachedSecUID(username); ok {
			vlog("secUid cache hit, skip embed")
			pages, err := fetchItemList(uid, username, 0, s)
			var t timings
			t.Pages = pages
			t.Total = time.Since(start)
			vlog("done videos=%d pages=%d total=%s", s.n, t.Pages, t.Total.Round(time.Millisecond))
			return s.n, t, err
		}
	}

	type secRes struct {
		uid string
		err error
	}
	secCh := make(chan secRes, 1)
	var secOnce sync.Once
	emit := func(u string) {
		first := s.n == 0
		s.add(u)
		if first && paginate {
			id := videoIDFromURL(u)
			secOnce.Do(func() {
				go func() {
					t0 := time.Now()
					uid, err := fetchSecUID(username, id)
					vlog("secUid %s err=%v", time.Since(t0).Round(time.Millisecond), err)
					secCh <- secRes{uid, err}
				}()
			})
		}
	}

	need := 10
	if limit > 0 && limit < 10 {
		need = limit
	}
	t, err := fetchEmbedURLs(username, need, emit)
	if err != nil && s.n == 0 {
		return 0, t, err
	}
	vlog("embed %s videos=%d", t.Download.Round(time.Millisecond)+t.TTFB.Round(time.Millisecond), s.n)

	if !paginate || s.n == 0 {
		t.Total = time.Since(start)
		t.Pages = 1
		return s.n, t, err
	}

	res := <-secCh
	if res.err != nil {
		t.Total = time.Since(start)
		return s.n, t, res.err
	}

	cursor := cursorFromVideoURL(s.last)
	pages, err := fetchItemList(res.uid, username, cursor, s)
	t.Pages = 1 + pages
	t.Total = time.Since(start)
	vlog("done videos=%d pages=%d total=%s", s.n, t.Pages, t.Total.Round(time.Millisecond))
	return s.n, t, err
}

func cursorFromVideoURL(u string) int64 {
	id, err := strconv.ParseUint(videoIDFromURL(u), 10, 64)
	if err != nil || id == 0 {
		return 0
	}
	return int64(id>>32) * 1000
}

func fetchEmbedURLs(username string, limit int, emit func(string)) (timings, error) {
	var t timings
	var dnsStart, connStart, tlsStart time.Time
	start := time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:           func(httptrace.DNSDoneInfo) { t.DNS = time.Since(dnsStart) },
		ConnectStart:      func(string, string) { connStart = time.Now() },
		ConnectDone:       func(string, string, error) { t.Connect = time.Since(connStart) },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(cs tls.ConnectionState, _ error) {
			t.TLS = time.Since(tlsStart)
			t.Resumed = cs.DidResume
		},
		GotFirstResponseByte: func() {
			t.TTFB = time.Since(start)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace),
		http.MethodGet,
		"https://www.tiktok.com/embed/@"+username,
		nil,
	)
	if err != nil {
		return t, err
	}
	setHeaders(req, "text/html")

	resp, err := doRequest(client, req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return t, fmt.Errorf("tiktok embed HTTP %d", resp.StatusCode)
	}

	bodyStart := time.Now()
	html, n := readUntilLimit(resp.Body, username, limit, emit)
	resp.Body.Close()
	t.Download = time.Since(bodyStart)

	parseStart := time.Now()
	if n < limit {
		for _, u := range extractFromJSON(html, username) {
			emit(u)
		}
	}
	t.Parse = time.Since(parseStart)
	return t, nil
}

func videoIDFromURL(u string) string {
	i := strings.LastIndex(u, "/video/")
	if i < 0 {
		return ""
	}
	id := u[i+len("/video/"):]
	if j := strings.IndexAny(id, "/?#"); j >= 0 {
		id = id[:j]
	}
	return id
}

func fetchSecUID(username, videoID string) (string, error) {
	if uid, ok := loadCachedSecUID(username); ok {
		vlog("secUid cache hit")
		return uid, nil
	}
	if videoID == "" {
		return "", fmt.Errorf("empty video id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.tiktok.com/embed/v2/"+videoID, nil)
	if err != nil {
		return "", err
	}
	setHeaders(req, "text/html")
	resp, err := doRequest(client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	var total int64
	for total < 8<<20 {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			total += int64(n)
			buf = append(buf, tmp[:n]...)
			if m := secUIDRE.FindSubmatch(buf); len(m) >= 2 {
				uid := string(m[1])
				saveCachedSecUID(username, uid)
				return uid, nil
			}
			if len(buf) > 8<<10 {
				keep := 256
				copy(buf, buf[len(buf)-keep:])
				buf = buf[:keep]
			}
		}
		if rerr != nil {
			break
		}
	}
	return "", fmt.Errorf("secUid not found")
}

func secUIDCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "tiktok_scraper", "secuid.json")
}

func loadCachedSecUID(username string) (string, bool) {
	raw, err := os.ReadFile(secUIDCachePath())
	if err != nil {
		return "", false
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	uid, ok := m[username]
	return uid, ok && uid != ""
}

func saveCachedSecUID(username, uid string) {
	path := secUIDCachePath()
	m := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	m[username] = uid
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}
