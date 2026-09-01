package explore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

var errGeoBlocked = errors.New("tiktok geo-blocked (HTTP 451)")

func isGeoBlocked(err error) bool {
	return err != nil && (errors.Is(err, errGeoBlocked) || strings.Contains(err.Error(), "HTTP 451"))
}

var universalDataRE = regexp.MustCompile(`<script id="__UNIVERSAL_DATA_FOR_REHYDRATION__"[^>]*>(.*?)</script>`)

type mintPageIDs struct {
	DeviceID      string
	OdinID        string
	WebIDLastTime string
	Region        string
}

var (
	webOnce sync.Once
	webC    tls_client.HttpClient
	webErr  error
)

func sharedWebClient() (tls_client.HttpClient, error) {
	webOnce.Do(func() {
		jar := tls_client.NewCookieJar()
		webC, webErr = tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithTimeoutSeconds(25),
			tls_client.WithClientProfile(profiles.Chrome_133),
			tls_client.WithCookieJar(jar),
		)
	})
	return webC, webErr
}

func mintSession(parent context.Context) (*pinnedSession, error) {
	fmt.Fprintln(os.Stderr, "minting msToken via Go (Chrome TLS)")

	_, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()

	client, err := sharedWebClient()
	if err != nil {
		return nil, fmt.Errorf("mint client: %w", err)
	}

	cookies := map[string]string{}
	if _, err = tlsGET(client, "https://www.tiktok.com/", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8", cookies); err != nil {
		return nil, fmt.Errorf("mint home: %w", err)
	}
	page, err := tlsGET(client, "https://www.tiktok.com/explore", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8", cookies)
	if err != nil {
		return nil, fmt.Errorf("mint explore: %w", err)
	}
	ids := parsePageIDs(page)
	if ids.DeviceID == "" {
		ids = generateWebIDs()
	}

	if _, err = tlsGET(client, "https://www.tiktok.com/api/recommend/item_list/?aid=1988&count=1&app_name=tiktok_web&device_platform=web_pc", "application/json", cookies); err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}

	q := url.Values{}
	q.Set("aid", "1988")
	q.Set("app_name", "tiktok_web")
	q.Set("channel", "tiktok_web")
	q.Set("device_platform", "web_pc")
	q.Set("categoryType", "104")
	q.Set("count", "12")
	q.Set("pullType", "2")
	q.Set("cookie_enabled", "true")
	q.Set("device_id", ids.DeviceID)
	if ids.OdinID != "" {
		q.Set("odinId", ids.OdinID)
	}
	if ids.WebIDLastTime != "" {
		q.Set("WebIdLastTime", ids.WebIDLastTime)
	}
	body, err := tlsGET(client, webItemListURL+"?"+q.Encode(), "application/json", cookies)
	if err != nil {
		return nil, fmt.Errorf("mint comedy: %w", err)
	}
	if webTokenDead(body, nil) {
		return nil, fmt.Errorf("mint: web comedy still empty (bytes=%d captcha=%v)", len(body), strings.Contains(string(body), "bdturing"))
	}

	harvestCookies(client, cookies)
	ck := cookieHeaderMap(cookies)
	if !strings.Contains(ck, "msToken=") {
		return nil, fmt.Errorf("mint: no msToken in cookies")
	}
	region := firstNonEmpty(normalizeRegion(cookies["store-country-code"]), normalizeRegion(ids.Region))
	return &pinnedSession{
		Cookie:        ck,
		DeviceID:      ids.DeviceID,
		OdinID:        ids.OdinID,
		WebIDLastTime: ids.WebIDLastTime,
		Region:        region,
		CSRF:          cookies["tt_csrf_token"],
		CapturedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func tlsGET(client tls_client.HttpClient, raw, accept string, cookies map[string]string) ([]byte, error) {
	body, token, err := tlsDo(client, raw, accept, "", cookies)
	if token != "" && cookies != nil {
		cookies["msToken"] = token
	}
	return body, err
}

func tlsDo(client tls_client.HttpClient, raw, accept, cookie string, cookies map[string]string) ([]byte, string, error) {
	if client == nil {
		var err error
		client, err = sharedWebClient()
		if err != nil {
			return nil, "", err
		}
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = http.Header{
		"accept":            {accept},
		"accept-language":   {"en-US,en;q=0.9,es;q=0.8"},
		"user-agent":        {webUA},
		"referer":           {"https://www.tiktok.com/explore"},
		http.HeaderOrderKey: {"accept", "accept-language", "user-agent", "referer"},
	}
	if cookie != "" {
		req.Header.Set("cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	if cookies != nil {
		for _, c := range resp.Cookies() {
			if c.Name != "" && c.Value != "" {
				cookies[c.Name] = c.Value
			}
		}
		for _, line := range resp.Header.Values("Set-Cookie") {
			nv, _, _ := strings.Cut(line, ";")
			k, v, ok := strings.Cut(nv, "=")
			if ok {
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				if k != "" && v != "" {
					cookies[k] = v
				}
			}
		}
	}
	token := strings.TrimSpace(resp.Header.Get("x-ms-token"))
	if resp.StatusCode == 451 {
		return rawBody, token, errGeoBlocked
	}
	if resp.StatusCode != 200 {
		return rawBody, token, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return rawBody, token, nil
}

func generateWebIDs() mintPageIDs {
	n := uint64(7_000_000_000_000_000_000) + uint64(rand.Int63n(899_999_999_999_999_999))
	return mintPageIDs{
		DeviceID:      fmt.Sprintf("%d", n),
		WebIDLastTime: fmt.Sprintf("%d", time.Now().Unix()),
	}
}

func harvestCookies(client tls_client.HttpClient, cookies map[string]string) {
	if client == nil || cookies == nil {
		return
	}
	u, err := url.Parse("https://www.tiktok.com/")
	if err != nil {
		return
	}
	for _, c := range client.GetCookies(u) {
		if c.Name != "" && c.Value != "" {
			cookies[c.Name] = c.Value
		}
	}
}

func parsePageIDs(html []byte) mintPageIDs {
	var ids mintPageIDs
	m := universalDataRE.FindSubmatch(html)
	if m == nil {
		return ids
	}
	var data map[string]any
	if json.Unmarshal(m[1], &data) != nil {
		return ids
	}
	scope, _ := data["__DEFAULT_SCOPE__"].(map[string]any)
	ctx, _ := scope["webapp.app-context"].(map[string]any)
	ids.DeviceID = stringify(ctx["wid"])
	ids.OdinID = stringify(ctx["odinId"])
	ids.WebIDLastTime = stringify(ctx["webIdCreatedTime"])
	ids.Region = stringify(ctx["region"])
	if user, ok := ctx["user"].(map[string]any); ok && ids.OdinID == "" {
		ids.OdinID = stringify(user["uid"])
	}
	return ids
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func cookieHeaderMap(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		if k == "" || v == "" {
			continue
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
