package profile

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	sessionCache = loadSessionCache()
	dnsStore     = loadDNSCache()
	client       = newFastClient()
)

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
