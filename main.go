package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxPages          = 10000
	pageWorkers       = 12
	guessSpacingItems = 10
	guessBudget       = 16
	markStride        = 13
	dnsPinTTL         = 24 * time.Hour
	dnsPinDialTimeout = 3 * time.Second
)

var (
	videoPathRE  = regexp.MustCompile(`/video/(\d{15,25})`)
	secUIDRE     = regexp.MustCompile(`"secUid":"(MS4wLjABAAAA[^"]+)"`)
	sessionCache = loadSessionCache()
	dnsStore     = loadDNSCache()
	client       = newFastClient()
	verbose      bool
)

func vlog(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

type stream struct {
	mu     sync.Mutex
	seen   map[string]struct{}
	n      int
	limit  int
	first  string
	last   string
	lastID string
	queue  chan string
	done   chan struct{}
}

func newStream(limit int) *stream {
	s := &stream{
		seen:  make(map[string]struct{}, 256),
		limit: limit,
		queue: make(chan string, 256),
		done:  make(chan struct{}),
	}
	go s.printer()
	return s
}

func (s *stream) printer() {
	defer close(s.done)
	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	i := 0
	flushed := true
	for {
		var u string
		var ok bool
		if flushed {
			u, ok = <-s.queue
		} else {
			select {
			case u, ok = <-s.queue:
			default:
				_ = out.Flush()
				flushed = true
				continue
			}
		}
		if !ok {
			_ = out.Flush()
			return
		}
		i++
		fmt.Fprintf(out, "%d. %s\n", i, u)
		flushed = false
	}
}

func (s *stream) more() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit <= 0 || s.n < s.limit
}

func (s *stream) add(u string) bool {
	if u == "" {
		return s.more()
	}
	s.mu.Lock()
	if s.limit > 0 && s.n >= s.limit {
		s.mu.Unlock()
		return false
	}
	if _, ok := s.seen[u]; ok {
		okMore := s.limit <= 0 || s.n < s.limit
		s.mu.Unlock()
		return okMore
	}
	s.seen[u] = struct{}{}
	s.n++
	if s.first == "" {
		s.first = u
	}
	s.last = u
	if i := strings.LastIndex(u, "/video/"); i >= 0 {
		s.lastID = u[i+len("/video/"):]
	}
	okMore := s.limit <= 0 || s.n < s.limit
	s.mu.Unlock()
	s.queue <- u
	return okMore
}

func (s *stream) remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit <= 0 {
		return -1
	}
	r := s.limit - s.n
	if r < 0 {
		r = 0
	}
	return r
}

func (s *stream) lastVideoID() string {
	s.mu.Lock()
	id := s.lastID
	s.mu.Unlock()
	return id
}

func (s *stream) finish() {
	close(s.queue)
	<-s.done
}

type frontityState struct {
	Source struct {
		Data map[string]json.RawMessage `json:"data"`
	} `json:"source"`
}

type embedPage struct {
	VideoList []embedVideo `json:"videoList"`
}

type embedVideo struct {
	ID             string `json:"id"`
	AuthorUniqueID string `json:"authorUniqueId"`
}

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

func main() {
	limit := flag.Int("limit", 0, "max videos to print (0 = all)")
	flag.BoolVar(&verbose, "v", false, "print per-request timing to stderr")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-limit N] [-v] <username|@username|url>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	username := normalizeUsername(flag.Arg(0))
	if username == "" {
		fmt.Fprintf(os.Stderr, "error: empty username\n")
		os.Exit(1)
	}

	n, t, err := fetchVideoURLs(username, *limit)
	sessionCache.flushNow()
	dnsStore.flushNow()
	if err != nil && n == 0 {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "error: no videos found for @%s\n", username)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if verbose {
		fmt.Fprintln(os.Stderr, t)
	}
}

func normalizeUsername(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.Contains(strings.ToLower(s), "tiktok.com") {
		toParse := s
		if !strings.Contains(s, "://") {
			toParse = "https://" + s
		}
		if u, err := url.Parse(toParse); err == nil {
			s = strings.Trim(u.Path, "/")
		}
	}
	s = strings.TrimPrefix(s, "@")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

type savedSession struct {
	Ticket []byte `json:"ticket"`
	State  []byte `json:"state"`
}

type diskSessionCache struct {
	mu             sync.Mutex
	mem            map[string]*tls.ClientSessionState
	path           string
	flushScheduled bool
}

func loadSessionCache() *diskSessionCache {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	c := &diskSessionCache{
		mem:  make(map[string]*tls.ClientSessionState),
		path: filepath.Join(dir, "tiktok_scraper", "tls-session.json"),
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return c
	}
	var stored map[string]savedSession
	if json.Unmarshal(raw, &stored) != nil {
		return c
	}
	for k, s := range stored {
		st, err := tls.ParseSessionState(s.State)
		if err != nil {
			continue
		}
		cs, err := tls.NewResumptionState(s.Ticket, st)
		if err != nil {
			continue
		}
		c.mem[k] = cs
	}
	return c
}

func (c *diskSessionCache) Get(sessionKey string) (*tls.ClientSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.mem[sessionKey]
	return st, ok
}

func (c *diskSessionCache) Put(sessionKey string, cs *tls.ClientSessionState) {
	c.mu.Lock()
	if cs == nil {
		delete(c.mem, sessionKey)
	} else {
		c.mem[sessionKey] = cs
	}
	if c.flushScheduled {
		c.mu.Unlock()
		return
	}
	c.flushScheduled = true
	c.mu.Unlock()
	go func() {
		time.Sleep(200 * time.Millisecond)
		c.mu.Lock()
		c.flushScheduled = false
		c.mu.Unlock()
		c.flushNow()
	}()
}

func (c *diskSessionCache) flushNow() {
	c.mu.Lock()
	stored := make(map[string]savedSession, len(c.mem))
	for k, st := range c.mem {
		ticket, state, err := st.ResumptionState()
		if err != nil || state == nil {
			continue
		}
		b, err := state.Bytes()
		if err != nil {
			continue
		}
		stored[k] = savedSession{Ticket: ticket, State: b}
	}
	path := c.path
	c.mu.Unlock()
	if len(stored) == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	raw, err := json.Marshal(stored)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}

type throttle struct {
	mu    sync.Mutex
	sem   chan struct{}
	until time.Time
	next  time.Time
}

var pace = &throttle{sem: make(chan struct{}, 4)}

func (t *throttle) waitCooldown(ctx context.Context) error {
	for {
		t.mu.Lock()
		d := time.Until(t.until)
		t.mu.Unlock()
		if d <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

func (t *throttle) acquire(ctx context.Context, urgent bool) (func(), error) {
	if err := t.waitCooldown(ctx); err != nil {
		return nil, err
	}
	if urgent {
		return func() {}, nil
	}
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-t.sem }
	t.mu.Lock()
	now := time.Now()
	if t.next.Before(now) {
		t.next = now
	}
	wait := t.next.Sub(now)
	t.next = t.next.Add(50 * time.Millisecond)
	t.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	}
	return release, nil
}

func (t *throttle) trip(d time.Duration) {
	t.mu.Lock()
	if u := time.Now().Add(d); u.After(t.until) {
		t.until = u
	}
	t.mu.Unlock()
}

type dnsEntry struct {
	IP string `json:"ip"`
	TS int64  `json:"ts"`
}

// dnsCache pins the last IP that answered for a host so a cold run can skip
// resolution entirely. A resolver hiccup then costs nothing instead of killing
// the run, which is what "Temporary failure in name resolution" used to do.
type dnsCache struct {
	mu    sync.Mutex
	m     map[string]dnsEntry
	path  string
	dirty bool
}

func loadDNSCache() *dnsCache {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	c := &dnsCache{
		m:    make(map[string]dnsEntry, 4),
		path: filepath.Join(dir, "tiktok_scraper", "dns.json"),
	}
	if raw, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(raw, &c.m)
	}
	return c
}

func (c *dnsCache) get(host string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[host]
	if !ok || e.IP == "" || time.Since(time.Unix(e.TS, 0)) > dnsPinTTL {
		return "", false
	}
	return e.IP, true
}

func (c *dnsCache) put(host, ip string) {
	if host == "" || ip == "" {
		return
	}
	c.mu.Lock()
	if e, ok := c.m[host]; !ok || e.IP != ip || time.Since(time.Unix(e.TS, 0)) > time.Hour {
		c.dirty = true
		c.m[host] = dnsEntry{IP: ip, TS: time.Now().Unix()}
	}
	c.mu.Unlock()
}

func (c *dnsCache) drop(host string) {
	if host == "" {
		return
	}
	c.mu.Lock()
	if _, ok := c.m[host]; ok {
		delete(c.m, host)
		c.dirty = true
	}
	c.mu.Unlock()
}

func (c *dnsCache) flushNow() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	c.dirty = false
	raw, err := json.Marshal(c.m)
	path := c.path
	c.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, raw, 0o600)
}

func dialPinned(ctx context.Context, d *net.Dialer, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.DialContext(ctx, "tcp4", addr)
	}
	if ip, ok := dnsStore.get(host); ok {
		pinCtx, cancel := context.WithTimeout(ctx, dnsPinDialTimeout)
		conn, err := d.DialContext(pinCtx, "tcp4", net.JoinHostPort(ip, port))
		cancel()
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		dnsStore.drop(host)
	}
	conn, err := d.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, err
	}
	if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok && ta.IP != nil {
		dnsStore.put(host, ta.IP.String())
	}
	return conn, nil
}

// doRequest drops the pinned IP on any transport failure, so a stale pin that
// still accepts TCP but fails TLS cannot outlive a single attempt.
func doRequest(c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil && req.Context().Err() == nil {
		dnsStore.drop(req.URL.Hostname())
	}
	return resp, err
}

func newFastClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialPinned(ctx, dialer, addr)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxConnsPerHost:       16,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 0,
			WriteBufferSize:       64 << 10,
			ReadBufferSize:        64 << 10,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				CurvePreferences:   []tls.CurveID{tls.X25519},
				ClientSessionCache: sessionCache,
			},
		},
	}
}

func setHeaders(req *http.Request, accept string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://www.tiktok.com/")
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

type itemListResp struct {
	ItemList []struct {
		ID         string `json:"id"`
		CreateTime int64  `json:"createTime"`
		Author     struct {
			UniqueID string `json:"uniqueId"`
		} `json:"author"`
	} `json:"itemList"`
	HasMorePrevious bool `json:"hasMorePrevious"`
	StatusCode      int  `json:"statusCode"`
}

func fetchItemList(secUID, username string, cursor int64, s *stream) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := newSpecState(username, s)
	defer spec.save()
	store := newBlockStore(spec)
	cache := newPageCache()
	jobs := make(chan int64, 512)
	priority := make(chan int64, 64)
	var queued sync.Map

	enqueue := func(c int64, urgent bool) {
		if _, loaded := queued.LoadOrStore(c, true); loaded {
			return
		}
		ch := jobs
		if urgent {
			ch = priority
		}
		select {
		case ch <- c:
		case <-ctx.Done():
		default:
			if urgent {
				select {
				case jobs <- c:
				case <-ctx.Done():
				default:
					queued.Delete(c)
				}
			} else {
				queued.Delete(c)
			}
		}
	}

	spec.enq = enqueue

	runJob := func(c int64, urgent bool) {
		items, hasMore, err := cache.get(ctx, secUID, c, store, client, urgent)
		if err != nil {
			if ctx.Err() == nil {
				vlog("worker cursor=%d err=%v", c, err)
			}
			return
		}
		if len(items) == 0 || !hasMore {
			return
		}
		enqueue(items[len(items)-1].CreateTime*1000, true)
	}

	var wg sync.WaitGroup
	for i := 0; i < pageWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case c, ok := <-priority:
					if !ok {
						return
					}
					runJob(c, true)
				default:
					select {
					case <-ctx.Done():
						return
					case c, ok := <-priority:
						if !ok {
							return
						}
						runJob(c, true)
					case c, ok := <-jobs:
						if !ok {
							return
						}
						runJob(c, false)
					}
				}
			}
		}()
	}
	spec.topUp()

	emitItems := func(batch []ttItem) int64 {
		var lastCreate int64
		for _, it := range batch {
			lastCreate = it.CreateTime
			if !s.add(itemURL(username, it)) {
				break
			}
		}
		return lastCreate
	}

	t0 := time.Now()
	page1, err := fetchItemListPage(ctx, secUID, cursor, client, true)
	pages := 1
	if err != nil {
		cancel()
		wg.Wait()
		return pages, err
	}
	items := page1.items()
	hasMore := page1.HasMorePrevious
	cache.put(cursor, items, hasMore)
	store.put(items, hasMore)
	if len(items) == 0 {
		vlog("page 1 %s empty", time.Since(t0).Round(time.Millisecond))
		cancel()
		wg.Wait()
		return pages, nil
	}

	before := s.n
	lastCreate := emitItems(items)
	vlog("page 1 %s +%d total=%d workers=%d hasMore=%t n=%d", time.Since(t0).Round(time.Millisecond), s.n-before, s.n, pageWorkers, hasMore, len(items))
	if !hasMore {
		cancel()
		wg.Wait()
		return pages, nil
	}
	enqueue(lastCreate*1000, true)

	emitOverlap := func(rest []ttItem, more bool) bool {
		before = s.n
		lastCreate = emitItems(rest)
		vlog("worker hit +%d total=%d", s.n-before, s.n)
		if !more || lastCreate == 0 || !s.more() {
			return false
		}
		enqueue(lastCreate*1000, true)
		return true
	}

	type pageGot struct {
		items []ttItem
		more  bool
		err   error
	}
	fallback := time.NewTicker(25 * time.Millisecond)
	defer fallback.Stop()

	misses := 0
	for s.more() && lastCreate != 0 && pages < maxPages {
		if rest, found, more := store.takeFrom(s.lastVideoID()); found && len(rest) > 0 {
			misses = 0
			if !emitOverlap(rest, more) {
				break
			}
			continue
		}

		next := lastCreate * 1000
		if next == cursor {
			break
		}

		got := make(chan pageGot, 1)
		t0 = time.Now()
		go func(cur int64) {
			it, hm, err := cache.get(ctx, secUID, cur, store, client, true)
			if (err != nil || len(it) == 0) && ctx.Err() == nil && s.more() {
				vlog("chain retry cursor=%d err=%v n=%d", cur, err, len(it))
				select {
				case <-time.After(800 * time.Millisecond):
				case <-ctx.Done():
					got <- pageGot{it, hm, err}
					return
				}
				p2, err2 := fetchItemListPage(ctx, secUID, cur, client, true)
				if err2 != nil {
					got <- pageGot{nil, false, err2}
					return
				}
				it2 := p2.items()
				if len(it2) > 0 {
					cache.put(cur, it2, p2.HasMorePrevious)
					store.put(it2, p2.HasMorePrevious)
				}
				got <- pageGot{it2, p2.HasMorePrevious, nil}
				return
			}
			got <- pageGot{it, hm, err}
		}(next)

		var g pageGot
		fromCache := false
		for {
			if rest, found, more := store.takeFrom(s.lastVideoID()); found && len(rest) > 0 {
				misses = 0
				if !emitOverlap(rest, more) {
					lastCreate = 0
				}
				break
			}
			select {
			case g = <-got:
				fromCache = true
			case <-store.notify:
				continue
			case <-fallback.C:
				continue
			case <-ctx.Done():
				lastCreate = 0
			}
			break
		}
		if !fromCache {
			continue
		}

		cursor = next
		pages++
		if g.err != nil || len(g.items) == 0 {
			misses++
			vlog("page %d %s miss=%d err=%v n=%d", pages, time.Since(t0).Round(time.Millisecond), misses, g.err, len(g.items))
			if s.n == 0 && g.err != nil {
				cancel()
				wg.Wait()
				return pages, g.err
			}
			if misses >= 24 {
				break
			}
			continue
		}
		misses = 0
		before = s.n
		lastCreate = emitItems(g.items)
		hasMore = g.more
		vlog("page %d %s +%d total=%d hasMore=%t n=%d", pages, time.Since(t0).Round(time.Millisecond), s.n-before, s.n, hasMore, len(g.items))
		if !hasMore {
			break
		}
		enqueue(lastCreate*1000, true)
	}

	cancel()
	wg.Wait()
	return pages, nil
}

type ttItem struct {
	ID         string
	Author     string
	CreateTime int64
}

func (p *itemListResp) items() []ttItem {
	out := make([]ttItem, 0, len(p.ItemList))
	for _, it := range p.ItemList {
		if it.ID == "" {
			continue
		}
		out = append(out, ttItem{ID: it.ID, Author: it.Author.UniqueID, CreateTime: it.CreateTime})
	}
	return out
}

func itemURL(username string, it ttItem) string {
	author := it.Author
	if author == "" {
		author = username
	}
	return "https://www.tiktok.com/@" + author + "/video/" + it.ID
}

type cadenceInfo struct {
	GapSec int64   `json:"gapSec"`
	Newest int64   `json:"newest"`
	Marks  []int64 `json:"marks,omitempty"`
}

func cadencePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "tiktok_scraper", "cadence.json")
}

type specState struct {
	mu        sync.Mutex
	username  string
	hint      int64
	anchor    int64
	minCT     int64
	maxCT     int64
	ids       map[string]struct{}
	cts       []int64
	marks     []int64
	frontGap  int64
	lastFront int64
	enq       func(int64, bool)
	s         *stream
}

func newSpecState(username string, s *stream) *specState {
	sp := &specState{username: username, s: s}
	if raw, err := os.ReadFile(cadencePath()); err == nil {
		var m map[string]cadenceInfo
		if json.Unmarshal(raw, &m) == nil {
			if c, ok := m[username]; ok {
				sp.hint = c.GapSec
				sp.anchor = c.Newest
				sp.marks = c.Marks
			}
		}
	}
	return sp
}

func (sp *specState) observe(items []ttItem) {
	sp.mu.Lock()
	var blkMin, blkMax int64
	n := 0
	for _, it := range items {
		if it.CreateTime <= 0 {
			continue
		}
		if blkMin == 0 || it.CreateTime < blkMin {
			blkMin = it.CreateTime
		}
		if it.CreateTime > blkMax {
			blkMax = it.CreateTime
		}
		n++
		if sp.ids == nil {
			sp.ids = make(map[string]struct{}, 256)
		}
		if _, ok := sp.ids[it.ID]; !ok {
			sp.ids[it.ID] = struct{}{}
			sp.cts = append(sp.cts, it.CreateTime)
		}
		if it.CreateTime > sp.maxCT {
			sp.maxCT = it.CreateTime
		}
	}
	if blkMin > 0 && (sp.minCT == 0 || blkMin < sp.minCT) {
		sp.minCT = blkMin
		if n >= 2 && blkMax > blkMin {
			sp.frontGap = (blkMax - blkMin) / int64(n-1)
		}
	}
	sp.mu.Unlock()
}

func (sp *specState) gapLocked() int64 {
	best := int64(0)
	take := func(g int64) {
		if g > 0 && (best == 0 || g < best) {
			best = g
		}
	}
	take(sp.frontGap)
	take(sp.hint)
	if cnt := int64(len(sp.ids)); cnt >= 2 && sp.maxCT > sp.minCT {
		take((sp.maxCT - sp.minCT) / (cnt - 1))
	}
	return best
}

func (sp *specState) topUp() {
	sp.mu.Lock()
	enq := sp.enq
	gap := sp.gapLocked()
	frontier := sp.minCT
	if frontier == 0 {
		frontier = sp.anchor
	}
	marks := sp.marks
	observed := len(sp.ids)
	skipExtrap := frontier > 0 && frontier == sp.lastFront
	if frontier > 0 {
		sp.lastFront = frontier
	}
	sp.mu.Unlock()
	if enq == nil {
		return
	}
	remaining := -1
	if sp.s != nil {
		remaining = sp.s.remaining()
	}
	if remaining == 0 {
		return
	}
	want := -1
	if remaining > 0 {
		want = remaining/markStride + 2
	}
	nMarks := len(marks)
	if want >= 0 && want < nMarks {
		nMarks = want
	}
	upTo := observed/markStride + 6
	if upTo > nMarks {
		upTo = nMarks
	}
	base := frontier
	for i := 0; i < upTo; i++ {
		enq((marks[i]+1)*1000, false)
		if base == 0 || marks[i] < base {
			base = marks[i]
		}
	}
	if upTo < nMarks || (want >= 0 && len(marks) >= want) {
		return
	}
	if skipExtrap || gap <= 0 || base <= 0 {
		return
	}
	n := guessBudget
	if remaining > 0 {
		if pages := remaining/guessSpacingItems + 1; pages < n {
			n = pages
		}
	}
	step := gap * guessSpacingItems
	if step < 1 {
		step = 1
	}
	for i := 1; i <= n; i++ {
		c := (base - int64(i)*step) * 1000
		if c < 1_400_000_000_000 {
			break
		}
		enq(c, false)
	}
}

func (sp *specState) save() {
	sp.mu.Lock()
	var gap int64
	if cnt := int64(len(sp.ids)); cnt >= 2 && sp.maxCT > sp.minCT {
		gap = (sp.maxCT - sp.minCT) / (cnt - 1)
	}
	newest := sp.maxCT
	user := sp.username
	minCT := sp.minCT
	cts := append([]int64(nil), sp.cts...)
	sp.mu.Unlock()
	if gap <= 0 || newest <= 0 || user == "" {
		return
	}
	sort.Slice(cts, func(i, j int) bool { return cts[i] > cts[j] })
	marks := make([]int64, 0, len(cts)/markStride+1)
	for i := markStride - 1; i < len(cts) && len(marks) < 400; i += markStride {
		marks = append(marks, cts[i])
	}
	path := cadencePath()
	m := map[string]cadenceInfo{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	for _, old := range m[user].Marks {
		if old < minCT && len(marks) < 400 {
			marks = append(marks, old)
		}
	}
	m[user] = cadenceInfo{GapSec: gap, Newest: newest, Marks: marks}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if raw, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(path, raw, 0o600)
	}
}

type pageCache struct {
	mu       sync.Mutex
	pages    map[int64][]ttItem
	hasMore  map[int64]bool
	inflight map[int64]chan struct{}
}

func newPageCache() *pageCache {
	return &pageCache{
		pages:    make(map[int64][]ttItem),
		hasMore:  make(map[int64]bool),
		inflight: make(map[int64]chan struct{}),
	}
}

func (c *pageCache) put(cursor int64, items []ttItem, hasMore bool) {
	c.mu.Lock()
	c.pages[cursor] = items
	c.hasMore[cursor] = hasMore
	c.mu.Unlock()
}

func (c *pageCache) get(ctx context.Context, secUID string, cursor int64, store *blockStore, hc *http.Client, urgent bool) ([]ttItem, bool, error) {
	c.mu.Lock()
	if items, ok := c.pages[cursor]; ok {
		hm := c.hasMore[cursor]
		c.mu.Unlock()
		return items, hm, nil
	}
	if ch, ok := c.inflight[cursor]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-ch:
		}
		c.mu.Lock()
		items := c.pages[cursor]
		hm := c.hasMore[cursor]
		c.mu.Unlock()
		return items, hm, nil
	}
	ch := make(chan struct{})
	c.inflight[cursor] = ch
	c.mu.Unlock()

	if hc == nil {
		hc = client
	}
	page, err := fetchItemListPage(ctx, secUID, cursor, hc, urgent)
	c.mu.Lock()
	delete(c.inflight, cursor)
	if err != nil {
		close(ch)
		c.mu.Unlock()
		return nil, false, err
	}
	items := page.items()
	if len(items) > 0 {
		c.pages[cursor] = items
		c.hasMore[cursor] = page.HasMorePrevious
	}
	close(ch)
	c.mu.Unlock()
	if len(items) > 0 {
		store.put(items, page.HasMorePrevious)
	}
	return items, page.HasMorePrevious, nil
}

type itemBlock struct {
	items []ttItem
	more  bool
}

type blockStore struct {
	mu     sync.Mutex
	blocks []itemBlock
	byID   map[string]int
	notify chan struct{}
	spec   *specState
}

func newBlockStore(spec *specState) *blockStore {
	return &blockStore{notify: make(chan struct{}, 1), spec: spec}
}

func (b *blockStore) put(items []ttItem, more bool) {
	if len(items) == 0 {
		return
	}
	if b.spec != nil {
		b.spec.observe(items)
	}
	b.mu.Lock()
	if b.byID == nil {
		b.byID = make(map[string]int, 128)
	}
	idx := len(b.blocks)
	b.blocks = append(b.blocks, itemBlock{items: items, more: more})
	for _, it := range items {
		b.byID[it.ID] = idx
	}
	b.mu.Unlock()
	if b.spec != nil {
		b.spec.topUp()
	}
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

func (b *blockStore) takeFrom(id string) (rest []ttItem, found bool, more bool) {
	if id == "" {
		return nil, false, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	idx, ok := b.byID[id]
	if !ok || idx < 0 || idx >= len(b.blocks) {
		return nil, false, true
	}
	blk := b.blocks[idx]
	for j, it := range blk.items {
		if it.ID != id {
			continue
		}
		rest = append([]ttItem(nil), blk.items[j+1:]...)
		for _, x := range blk.items {
			delete(b.byID, x.ID)
		}
		last := len(b.blocks) - 1
		if idx != last {
			b.blocks[idx] = b.blocks[last]
			for _, x := range b.blocks[idx].items {
				b.byID[x.ID] = idx
			}
		}
		b.blocks = b.blocks[:last]
		return rest, true, blk.more
	}
	return nil, false, true
}

func fetchItemListPage(ctx context.Context, secUID string, cursor int64, c *http.Client, urgent bool) (*itemListResp, error) {
	var b strings.Builder
	b.Grow(160 + len(secUID))
	b.WriteString("https://www.tiktok.com/api/creator/item_list/?aid=1988&count=15&cursor=")
	b.WriteString(strconv.FormatInt(cursor, 10))
	b.WriteString("&secUid=")
	b.WriteString(secUID)
	b.WriteString("&type=1")
	u := b.String()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(200*attempt) * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		release, err := pace.acquire(ctx, urgent)
		if err != nil {
			return nil, err
		}
		page, retry, err := doItemListRequest(ctx, u, c)
		release()
		if err == nil {
			// Only the chain retries an empty page: a speculative cursor past the
			// end of history is legitimately empty, and retrying it burns requests.
			if !urgent || len(page.items()) > 0 || attempt >= 2 {
				return page, nil
			}
			lastErr = fmt.Errorf("item_list empty")
			continue
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, lastErr
}

func doItemListRequest(ctx context.Context, u string, c *http.Client) (*itemListResp, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	setHeaders(req, "application/json")
	resp, err := doRequest(c, req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			pace.trip(1500 * time.Millisecond)
			return nil, true, fmt.Errorf("item_list HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode >= 500, fmt.Errorf("item_list HTTP %d", resp.StatusCode)
	}
	var page itemListResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page); err != nil {
		return nil, true, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if page.StatusCode != 0 {
		if page.StatusCode == 10102 {
			pace.trip(1500 * time.Millisecond)
			return nil, true, fmt.Errorf("item_list status %d", page.StatusCode)
		}
		return nil, false, fmt.Errorf("item_list status %d", page.StatusCode)
	}
	return &page, false, nil
}

func readUntilLimit(r io.Reader, username string, limit int, emit func(string)) ([]byte, int) {
	buf := bytes.NewBuffer(make([]byte, 0, 64<<10))
	tmp := make([]byte, 16<<10)
	seen := make(map[string]struct{}, limit)
	n := 0
	scanFrom := 0

	for n < limit {
		c, err := r.Read(tmp)
		if c > 0 {
			buf.Write(tmp[:c])
			b := buf.Bytes()
			n, scanFrom = pullIDs(b, scanFrom, username, seen, n, limit, emit)
		}
		if err != nil {
			break
		}
	}
	return buf.Bytes(), n
}

func pullIDs(body []byte, from int, username string, seen map[string]struct{}, n, limit int, emit func(string)) (int, int) {
	if from > 64 {
		from -= 64
	} else {
		from = 0
	}
	matches := videoPathRE.FindAllSubmatch(body[from:], -1)
	for _, m := range matches {
		id := string(m[1])
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		emit("https://www.tiktok.com/@" + username + "/video/" + id)
		n++
		if n >= limit {
			break
		}
	}
	return n, len(body)
}

func extractFromJSON(html []byte, username string) []string {
	const marker = `id="__FRONTITY_CONNECT_STATE__"`
	i := bytes.Index(html, []byte(marker))
	if i < 0 {
		return nil
	}
	gt := bytes.IndexByte(html[i:], '>')
	if gt < 0 {
		return nil
	}
	start := i + gt + 1
	end := bytes.Index(html[start:], []byte("</script>"))
	if end < 0 {
		return nil
	}

	var state frontityState
	if err := json.Unmarshal(html[start:start+end], &state); err != nil {
		return nil
	}

	var videos []embedVideo
	for key, raw := range state.Source.Data {
		if !strings.Contains(key, username) {
			continue
		}
		var page embedPage
		if err := json.Unmarshal(raw, &page); err != nil {
			continue
		}
		if len(page.VideoList) > 0 {
			videos = page.VideoList
			break
		}
	}

	urls := make([]string, 0, len(videos))
	seen := make(map[string]struct{}, len(videos))
	for _, v := range videos {
		if v.ID == "" {
			continue
		}
		if _, ok := seen[v.ID]; ok {
			continue
		}
		seen[v.ID] = struct{}{}
		author := v.AuthorUniqueID
		if author == "" {
			author = username
		}
		urls = append(urls, "https://www.tiktok.com/@"+author+"/video/"+v.ID)
	}
	return urls
}
