package explore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"time"
)

const exploreStall = 20 * time.Second

func stream(ctx context.Context, first []byte, next func(cursor string, fresh bool) ([]byte, error), parse func([]byte) ([]video, string, error), dump bool, limit int, sessPath string, sess *pinnedSession) int {
	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	defer out.Flush()
	seen := make(map[string]struct{}, 256)
	n := 0
	page := 0
	body := first
	backoff := time.Duration(0)
	cursor := pageCursor(first)
	lastFresh := time.Time{}
	dupePages := 0
	sameCursor := 0
	prevCur := cursor

	for {
		fresh := false
		if dump {
			if _, err := out.Write(body); err != nil {
				return n
			}
			if len(body) == 0 || body[len(body)-1] != '\n' {
				_ = out.WriteByte('\n')
			}
			_ = out.Flush()
		} else {
			vids, summary, err := parse(body)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				if page == 0 {
					os.Exit(1)
				}
				backoff = 2 * time.Second
			} else {
				if page == 0 {
					fmt.Fprintln(os.Stderr, summary)
				}
				added := 0
				for _, v := range vids {
					if v.URL == "" {
						continue
					}
					if _, ok := seen[v.URL]; ok {
						continue
					}
					seen[v.URL] = struct{}{}
					n++
					added++
					fmt.Fprintf(out, "%d. %s\n", n, v.URL)
					if limit > 0 && n >= limit {
						_ = out.Flush()
						return n
					}
				}
				_ = out.Flush()
				cur := pageCursor(body)
				if added > 0 {
					dupePages = 0
					sameCursor = 0
				} else if page > 0 {
					dupePages++
					if cur == "" || cur == prevCur {
						sameCursor++
					} else {
						sameCursor = 0
					}
					if dupePages%25 == 0 {
						fmt.Fprintf(os.Stderr, "explore: skipped %d duplicate pages, paging\n", dupePages)
					}
					ended := !pageHasMore(body) || len(vids) == 0
					stuck := sameCursor >= 8
					if canFresh(lastFresh) && (ended || stuck) {
						fmt.Fprintln(os.Stderr, "explore stalled; fetching a new feed")
						fresh = true
						cursor = ""
						dupePages = 0
						sameCursor = 0
						backoff = 2 * time.Second
					}
				}
				if cur != "" {
					prevCur = cur
				}
			}
		}
		saveSession(sessPath, sess)
		if !fresh {
			if c := pageCursor(body); c != "" {
				cursor = c
			}
		}
		page++

		wait := 500 * time.Millisecond
		if backoff > 0 {
			wait = backoff
			backoff = 0
		}
		select {
		case <-ctx.Done():
			return n
		case <-time.After(wait):
		}

		wantFresh := fresh
		var err error
		for {
			body, err = next(cursor, fresh)
			if wantFresh {
				lastFresh = time.Now()
			}
			fresh = false
			wantFresh = false
			if err == nil && len(body) > 0 && !bytes.Contains(body, []byte("bdturing-verify")) {
				backoff = 0
				break
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			} else {
				fmt.Fprintln(os.Stderr, "empty page, retrying")
			}
			if backoff == 0 {
				backoff = time.Second
			} else if backoff < 8*time.Second {
				backoff *= 2
			}
			select {
			case <-ctx.Done():
				return n
			case <-time.After(backoff):
			}
		}
	}
}

func canFresh(lastFresh time.Time) bool {
	return lastFresh.IsZero() || time.Since(lastFresh) >= exploreStall
}
