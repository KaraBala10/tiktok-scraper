package explore

type webResp struct {
	HasMore    bool   `json:"hasMore"`
	StatusCode int    `json:"statusCode"`
	ItemList   []item `json:"itemList"`
}

type item struct {
	ID           string `json:"id"`
	Desc         string `json:"desc"`
	CategoryType int    `json:"CategoryType"`
	TextLanguage string `json:"textLanguage"`
	Author       struct {
		UniqueID string `json:"uniqueId"`
		Nickname string `json:"nickname"`
	} `json:"author"`
}

type video struct {
	URL    string
	User   string
	Region string
	Desc   string
}

type pinnedSession struct {
	Region        string `json:"region"`
	DeviceID      string `json:"device_id"`
	OdinID        string `json:"odin_id"`
	WebIDLastTime string `json:"web_id_last_time"`
	CSRF          string `json:"csrf_token,omitempty"`
	Cookie        string `json:"cookie"`
	CapturedAt    string `json:"captured_at"`
}
