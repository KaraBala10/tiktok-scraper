package explore

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed regions.json categories.json
var dataFS embed.FS

const (
	webItemListURL = "https://www.tiktok.com/api/explore/item_list/"
	webUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

var defaultCategories = map[string]int{
	"anime": 100, "shows": 101, "beauty": 102, "games": 103,
	"comedy": 104, "daily": 105, "family": 106, "relationship": 107,
	"drama": 108, "outfit": 109, "lipsync": 110, "food": 111,
	"sports": 112, "animals": 113, "society": 114, "cars": 115,
	"education": 116, "fitness": 117, "technology": 118, "singing": 119,
	"all": 120,
}

type regionProfile struct {
	Code     string `json:"-"`
	Language string `json:"language"`
	Timezone string `json:"timezone"`
}

func loadRegion(code string) (regionProfile, error) {
	b, err := readConfig("regions.json")
	if err != nil {
		return regionProfile{}, fmt.Errorf("regions.json: %w", err)
	}
	var all map[string]regionProfile
	if err := json.Unmarshal(b, &all); err != nil {
		return regionProfile{}, err
	}
	p, ok := all[code]
	if !ok {
		return regionProfile{}, fmt.Errorf("unknown region %q", code)
	}
	p.Code = code
	return p, nil
}

func resolveCategory(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	cats := defaultCategories
	if b, err := readConfig("categories.json"); err == nil {
		var extra map[string]int
		if json.Unmarshal(b, &extra) == nil && len(extra) > 0 {
			cats = extra
		}
	}
	if n, ok := cats[s]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("unknown category %q (try comedy or 104)", s)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "?"
	}
	return s
}

func formatCounts(m map[string]int) string {
	type kv struct {
		k string
		n int
	}
	list := make([]kv, 0, len(m))
	for k, n := range m {
		list = append(list, kv{k, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].k < list[j].k
	})
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, fmt.Sprintf("%s:%d", x.k, x.n))
	}
	return strings.Join(parts, ",")
}

func readConfig(name string) ([]byte, error) {
	if b, err := os.ReadFile(name); err == nil {
		return b, nil
	}
	return dataFS.ReadFile(name)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
