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

func fetchWeb(categoryType, count int, p regionProfile, sess *pinnedSession) ([]byte, http.Header, error) {
	code := p.Code
	q := url.Values{}
	q.Set("aid", "1988")
	q.Set("app_name", "tiktok_web")
	q.Set("channel", "tiktok_web")
	q.Set("device_platform", "web_pc")
	q.Set("categoryType", strconv.Itoa(categoryType))
	q.Set("count", strconv.Itoa(count))
	q.Set("pullType", "2")
	q.Set("cookie_enabled", "true")
	q.Set("os", "linux")
	q.Set("region", code)
	q.Set("priority_region", code)
	q.Set("carrier_region", code)
	q.Set("tz_name", p.Timezone)
	q.Set("language", p.Language)
	q.Set("app_language", p.Language)
	q.Set("webcast_language", p.Language)
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

func fetchWebComedy(categoryType, count int, p regionProfile, sess *pinnedSession) ([]byte, error) {
	body, hdr, err := fetchWeb(categoryType, count, p, sess)
	if err != nil {
		return nil, err
	}
	if next := hdr.Get("x-ms-token"); next != "" {
		sess.Cookie = mergeMsToken(sess.Cookie, next)
		if len(body) == 0 || webEmpty(body) {
			body, hdr, err = fetchWeb(categoryType, count, p, sess)
			if err != nil {
				return nil, err
			}
			if n := hdr.Get("x-ms-token"); n != "" {
				sess.Cookie = mergeMsToken(sess.Cookie, n)
			}
		}
	}
	if !webTokenDead(body, nil) {
		sess.CapturedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return body, nil
}

func parseWeb(body []byte, cat int, p regionProfile) ([]video, string, error) {
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
			URL:  "https://www.tiktok.com/@" + user + "/video/" + it.ID,
			User: user,
			Desc: it.Desc,
		})
	}
	return out, fmt.Sprintf("source=web-comedy categoryType=%d geo=ip items=%d langs=%s", cat, len(out), formatCounts(langs)), nil
}

func webTokenDead(body []byte, err error) bool {
	if err != nil {
		return true
	}
	if len(body) == 0 {
		return true
	}
	if bytes.Contains(body, []byte("bdturing-verify")) {
		return true
	}
	var page webResp
	if json.Unmarshal(body, &page) != nil {
		return true
	}
	if page.StatusCode != 0 {
		return true
	}
	return len(page.ItemList) == 0
}

func webEmpty(body []byte) bool {
	return webTokenDead(body, nil)
}
