package explore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
