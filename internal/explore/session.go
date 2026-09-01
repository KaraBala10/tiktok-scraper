package explore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "tiktok_scraper")
}

func defaultSessionPath() string {
	return filepath.Join(cacheDir(), "session.json")
}

func defaultCookieFile() string {
	return filepath.Join(cacheDir(), "cookies.txt")
}

func detectIPRegion() string {
	if r := regionFromTrace("https://www.cloudflare.com/cdn-cgi/trace"); r != "" {
		return r
	}
	if r := regionFromIPAPI(); r != "" {
		return r
	}
	return ""
}

func regionFromTrace(rawURL string) string {
	body, err := httpGet(rawURL, 4*time.Second)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "loc" {
			return normalizeRegion(v)
		}
	}
	return ""
}

func regionFromIPAPI() string {
	body, err := httpGet("http://ip-api.com/json/?fields=status,countryCode", 4*time.Second)
	if err != nil {
		return ""
	}
	var out struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
	}
	if json.Unmarshal(body, &out) != nil || out.Status != "success" {
		return ""
	}
	return normalizeRegion(out.CountryCode)
}

func httpGet(rawURL string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<16))
}

func normalizeRegion(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 {
		return ""
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return ""
		}
	}
	return s
}

func loadCookieHeader(path string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("MSTOKEN")); v != "" {
		if !strings.Contains(v, "=") {
			return "msToken=" + v, nil
		}
		return v, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", fmt.Errorf("empty %s", path)
	}
	var tokens []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "\t") {
			parts := strings.Split(line, "\t")
			if len(parts) >= 7 && parts[5] == "msToken" {
				tokens = append(tokens, "msToken="+parts[6])
			}
			continue
		}
		if strings.HasPrefix(line, "Cookie:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "Cookie:"))
		}
		tokens = append(tokens, line)
	}
	if len(tokens) == 0 {
		return "", fmt.Errorf("no cookies in %s", path)
	}
	return strings.Join(tokens, "; "), nil
}

func loadDeviceID(cookieFile string) string {
	seen := map[string]bool{}
	paths := []string{filepath.Join(cacheDir(), "device_id.txt")}
	if cookieFile != "" {
		paths = append(paths, filepath.Join(filepath.Dir(cookieFile), "device_id.txt"))
	}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	return ""
}

func loadSession(path string) *pinnedSession {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s pinnedSession
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	if s.Cookie == "" && s.DeviceID == "" {
		return nil
	}
	return &s
}

func saveSession(path string, s *pinnedSession) {
	if s == nil {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
	if s.Cookie != "" {
		_ = os.WriteFile(filepath.Join(dir, "cookies.txt"), []byte(s.Cookie+"\n"), 0o600)
	}
	if s.DeviceID != "" {
		_ = os.WriteFile(filepath.Join(dir, "device_id.txt"), []byte(s.DeviceID+"\n"), 0o600)
	}
}

func mergeMsToken(cookie, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return cookie
	}
	parts := []string{}
	for _, p := range strings.Split(cookie, ";") {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "msToken=") {
			continue
		}
		parts = append(parts, p)
	}
	parts = append(parts, "msToken="+token)
	return strings.Join(parts, "; ")
}
