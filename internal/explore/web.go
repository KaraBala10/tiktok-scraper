package explore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func fetchWeb(categoryType, count int, p regionProfile, sess *pinnedSession, cursor string, fresh bool) ([]byte, http.Header, error) {
	code := p.Code
	q := url.Values{}
	q.Set("aid", "1988")
	q.Set("app_name", "tiktok_web")
	q.Set("channel", "tiktok_web")
	q.Set("device_platform", "web_pc")
	q.Set("categoryType", strconv.Itoa(categoryType))
	q.Set("count", strconv.Itoa(count))
	if fresh {
		q.Set("pullType", "1")
	} else {
		q.Set("pullType", "2")
	}
	q.Set("cookie_enabled", "true")
	q.Set("os", "linux")
	q.Set("region", code)
	q.Set("priority_region", code)
	q.Set("carrier_region", code)
	q.Set("tz_name", p.Timezone)
	q.Set("language", p.Language)
	q.Set("app_language", p.Language)
	q.Set("webcast_language", p.Language)
	if cursor != "" && !fresh {
		q.Set("cursor", cursor)
	}
	if sess != nil && sess.DeviceID == "" {
		ids := generateWebIDs()
		sess.DeviceID = ids.DeviceID
		if sess.WebIDLastTime == "" {
			sess.WebIDLastTime = ids.WebIDLastTime
		}
	}
	if sess != nil && sess.DeviceID != "" {
		q.Set("device_id", sess.DeviceID)
	}
	if sess != nil && sess.OdinID != "" {
		q.Set("odinId", sess.OdinID)
	}
	if sess != nil && sess.WebIDLastTime != "" {
		q.Set("WebIdLastTime", sess.WebIDLastTime)
	}
	ck := ""
	if sess != nil {
		ck = sess.Cookie
	}
	if ck != "" && sess != nil && sess.Region != "" && !strings.Contains(ck, "store-country-code=") {
		ck = ck + "; store-country-code=" + strings.ToLower(sess.Region) + "; store-country-code-src=uid"
	}
	raw, token, err := tlsDo(nil, webItemListURL+"?"+q.Encode(), "application/json", ck, nil)
	hdr := make(http.Header)
	if token != "" {
		hdr.Set("x-ms-token", token)
	}
	if err != nil {
		return raw, hdr, err
	}
	return raw, hdr, nil
}

func fetchWebComedy(categoryType, count int, p regionProfile, sess *pinnedSession, cursor string, fresh bool) ([]byte, error) {
	body, hdr, err := fetchWeb(categoryType, count, p, sess, cursor, fresh)
	if err != nil {
		return nil, err
	}
	if next := hdr.Get("x-ms-token"); next != "" {
		sess.Cookie = mergeMsToken(sess.Cookie, next)
	}
	if len(body) == 0 || webEmpty(body) {
		body, hdr, err = fetchWeb(categoryType, count, p, sess, cursor, fresh)
		if err != nil {
			return nil, err
		}
		if n := hdr.Get("x-ms-token"); n != "" {
			sess.Cookie = mergeMsToken(sess.Cookie, n)
		}
	}
	if !webTokenDead(body, nil) {
		sess.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return body, nil
}

func pageCursor(body []byte) string {
	var page struct {
		Cursor json.RawMessage `json:"cursor"`
	}
	if json.Unmarshal(body, &page) != nil || len(page.Cursor) == 0 || string(page.Cursor) == "null" {
		return ""
	}
	s := strings.TrimSpace(string(page.Cursor))
	s = strings.Trim(s, `"`)
	if s == "" || s == "0" {
		return ""
	}
	return s
}

func parseWeb(body []byte, cat int) ([]video, string, error) {
	if len(body) == 0 {
		return nil, "", fmt.Errorf("empty body (slide verify); token mint failed")
	}
	var page webResp
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, "", fmt.Errorf("json: %w", err)
	}
	if page.StatusCode != 0 {
		return nil, "", fmt.Errorf("statusCode=%d items=%d", page.StatusCode, len(page.ItemList))
	}
	langs := map[string]int{}
	out := make([]video, 0, len(page.ItemList))
	for _, it := range page.ItemList {
		user := it.Author.UniqueID
		if user == "" {
			user = "unknown"
		}
		lang := it.TextLanguage
		if lang == "" {
			lang = "?"
		}
		langs[lang]++
		out = append(out, video{
			URL: "https://www.tiktok.com/@" + user + "/video/" + it.ID,
		})
	}
	return out, fmt.Sprintf("source=web-comedy categoryType=%d geo=ip items=%d langs=%s", cat, len(out), formatCounts(langs)), nil
}

func webBadBody(body []byte) bool {
	if len(body) == 0 || bytes.Contains(body, []byte("bdturing-verify")) {
		return true
	}
	var page webResp
	return json.Unmarshal(body, &page) != nil
}

func webTokenDead(body []byte, err error) bool {
	if isGeoBlocked(err) {
		return false
	}
	if err != nil {
		return true
	}
	return webBadBody(body) || webLoginDead(body)
}

func webEmpty(body []byte) bool {
	if webBadBody(body) {
		return true
	}
	var page webResp
	if json.Unmarshal(body, &page) != nil {
		return true
	}
	return page.StatusCode != 0 || len(page.ItemList) == 0
}

func webLoginDead(body []byte) bool {
	var page webResp
	if json.Unmarshal(body, &page) != nil {
		return false
	}
	return page.StatusCode == 10000 || page.StatusCode == 100001
}

func pageHasMore(body []byte) bool {
	var page webResp
	if json.Unmarshal(body, &page) != nil {
		return true
	}
	return page.HasMore
}
