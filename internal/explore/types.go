package explore

type webResp struct {
	HasMore    bool   `json:"hasMore"`
	StatusCode int    `json:"statusCode"`
	ItemList   []item `json:"itemList"`
}

type item struct {
	ID           string `json:"id"`
	TextLanguage string `json:"textLanguage"`
	Author       struct {
		UniqueID string `json:"uniqueId"`
	} `json:"author"`
}

type video struct {
	URL string
}

type pinnedSession struct {
	Region        string `json:"region"`
	DeviceID      string `json:"device_id"`
	OdinID        string `json:"odin_id"`
	WebIDLastTime string `json:"web_id_last_time"`
	Cookie        string `json:"cookie"`
	CapturedAt    string `json:"captured_at"`
}
