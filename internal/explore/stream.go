package explore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"time"
)

func stream(ctx context.Context, first []byte, next func() ([]byte, error), parse func([]byte) ([]video, string, error), dump bool, limit int, sessPath string, sess *pinnedSession, savePin *bool) int {
	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	defer out.Flush()
	seen := make(map[string]struct{}, 256)
	n := 0
	page := 0
	body := first
	backoff := time.Duration(0)

	for {
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
				if added == 0 && page > 0 {
					backoff = 2 * time.Second
				}
			}
		}
		if savePin != nil && *savePin {
			saveSession(sessPath, sess)
		}
		page++

		wait := 400 * time.Millisecond
		if backoff > 0 {
			wait = backoff
			backoff = 0
		}
		select {
		case <-ctx.Done():
			return n
		case <-time.After(wait):
		}

		var err error
		for {
			body, err = next()
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
