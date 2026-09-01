package profile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
