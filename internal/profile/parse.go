package profile

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

var videoPathRE = regexp.MustCompile(`/video/(\d{15,25})`)

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
