package profile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

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
