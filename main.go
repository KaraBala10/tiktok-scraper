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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxPages    = 10000
	pageWorkers = 10
)

var (
	videoPathRE  = regexp.MustCompile(`/video/(\d{15,25})`)
	secUIDRE     = regexp.MustCompile(`"secUid":"(MS4wLjABAAAA[^"]+)"`)
	sessionCache = loadSessionCache()
	client       = newFastClient()
	verbose      bool
)

func init() {
	go warmTLS(client)
}

func warmTLS(c *http.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.tiktok.com/favicon.ico", nil)
	if err != nil {
		return
	}
	setHeaders(req, "*/*")
	resp, err := c.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func vlog(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

type stream struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	n     int
	limit int
	first string
	last  string
	queue chan string
	done  chan struct{}
}

func newStream(limit int) *stream {
	s := &stream{
		seen:  make(map[string]struct{}, 64),
		limit: limit,
		queue: make(chan string, 256),
		done:  make(chan struct{}),
	}
	go s.printer()
	return s
}

func (s *stream) printer() {
	defer close(s.done)
	out := bufio.NewWriterSize(os.Stdout, 4096)
	i := 0
	for u := range s.queue {
		i++
		fmt.Fprintf(out, "%d. %s\n", i, u)
		_ = out.Flush()
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
	okMore := s.limit <= 0 || s.n < s.limit
	s.mu.Unlock()
	s.queue <- u
	return okMore
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
	mu   sync.Mutex
	mem  map[string]*tls.ClientSessionState
	path string
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
	go func() {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		raw, err := json.Marshal(stored)
		if err != nil {
			return
		}
		_ = os.WriteFile(path, raw, 0o600)
	}()
}

func newFastClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   4 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxConnsPerHost:       16,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 0,
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

	resp, err := client.Do(req)
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
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	m := secUIDRE.FindSubmatch(body)
	if len(m) < 2 {
		return "", fmt.Errorf("secUid not found")
	}
	uid := string(m[1])
	saveCachedSecUID(username, uid)
	return uid, nil
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

	store := &blockStore{}
	cache := newPageCache()
	jobs := make(chan int64, 256)
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

	runJob := func(c int64) {
		items, hasMore, err := cache.get(ctx, secUID, c, store, client)
		if err != nil || len(items) == 0 || !hasMore {
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
					runJob(c)
				default:
					select {
					case <-ctx.Done():
						return
					case c, ok := <-priority:
						if !ok {
							return
						}
						runJob(c)
					case c, ok := <-jobs:
						if !ok {
							return
						}
						runJob(c)
					}
				}
			}
		}()
	}

	t0 := time.Now()
	page1, err := fetchItemListPage(ctx, secUID, cursor, client)
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

	before := s.n
	lastCreate := emitItems(items)
	vlog("page 1 %s +%d total=%d workers=%d hasMore=%t n=%d", time.Since(t0).Round(time.Millisecond), s.n-before, s.n, pageWorkers, hasMore, len(items))
	if !hasMore {
		cancel()
		wg.Wait()
		return pages, nil
	}
	for _, g := range guessFromItems(items) {
		enqueue(g, false)
	}
	enqueue(lastCreate*1000, true)

	for s.more() && lastCreate != 0 {
		if rest, found, more := store.takeFrom(videoIDFromURL(s.last)); found && len(rest) > 0 {
			before = s.n
			lastCreate = emitItems(rest)
			vlog("worker hit +%d total=%d", s.n-before, s.n)
			if !more || lastCreate == 0 || !s.more() {
				break
			}
			enqueue(lastCreate*1000, true)
			continue
		}

		next := lastCreate * 1000
		if next == cursor {
			break
		}
		cursor = next
		t0 = time.Now()
		items, hasMore, err = cache.get(ctx, secUID, cursor, store, client)
		pages++
		if err != nil {
			if s.n > 0 {
				break
			}
			cancel()
			wg.Wait()
			return pages, err
		}
		if len(items) == 0 {
			vlog("page %d %s empty", pages, time.Since(t0).Round(time.Millisecond))
			break
		}
		before = s.n
		lastCreate = emitItems(items)
		vlog("page %d %s +%d total=%d hasMore=%t n=%d", pages, time.Since(t0).Round(time.Millisecond), s.n-before, s.n, hasMore, len(items))
		if !hasMore {
			break
		}
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

func guessFromItems(items []ttItem) []int64 {
	if len(items) < 2 {
		return nil
	}
	newest := items[0].CreateTime
	oldest := items[len(items)-1].CreateTime
	span := newest - oldest
	if span <= 0 {
		return nil
	}
	step := span / 3
	if step < 1 {
		step = 1
	}
	out := make([]int64, 0, 12)
	for i := 1; i <= 12; i++ {
		g := (oldest - int64(i)*step) * 1000
		if g > 0 {
			out = append(out, g)
		}
	}
	return out
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

func (c *pageCache) get(ctx context.Context, secUID string, cursor int64, store *blockStore, hc *http.Client) ([]ttItem, bool, error) {
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
	page, err := fetchItemListPage(ctx, secUID, cursor, hc)
	c.mu.Lock()
	delete(c.inflight, cursor)
	if err != nil {
		close(ch)
		c.mu.Unlock()
		return nil, false, err
	}
	items := page.items()
	c.pages[cursor] = items
	c.hasMore[cursor] = page.HasMorePrevious
	close(ch)
	c.mu.Unlock()
	store.put(items, page.HasMorePrevious)
	return items, page.HasMorePrevious, nil
}

type itemBlock struct {
	items []ttItem
	more  bool
}

type blockStore struct {
	mu     sync.Mutex
	blocks []itemBlock
}

func (b *blockStore) put(items []ttItem, more bool) {
	if len(items) == 0 {
		return
	}
	b.mu.Lock()
	b.blocks = append(b.blocks, itemBlock{items: items, more: more})
	b.mu.Unlock()
}

func (b *blockStore) takeFrom(id string) (rest []ttItem, found bool, more bool) {
	if id == "" {
		return nil, false, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, blk := range b.blocks {
		for j, it := range blk.items {
			if it.ID != id {
				continue
			}
			rest = append([]ttItem(nil), blk.items[j+1:]...)
			b.blocks = append(b.blocks[:i], b.blocks[i+1:]...)
			return rest, true, blk.more
		}
	}
	return nil, false, true
}

func fetchItemListPage(ctx context.Context, secUID string, cursor int64, c *http.Client) (*itemListResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	u := fmt.Sprintf(
		"https://www.tiktok.com/api/creator/item_list/?aid=1988&count=15&cursor=%d&secUid=%s&type=1",
		cursor, secUID,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("item_list HTTP %d", resp.StatusCode)
	}
	var page itemListResp
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, err
	}
	if page.StatusCode != 0 {
		return nil, fmt.Errorf("item_list status %d", page.StatusCode)
	}
	return &page, nil
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
