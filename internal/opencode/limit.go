package opencode

import "encoding/json"

// Limit is a usage-limit failure as OpenCode reports it for meridian's rate-limit error.
type Limit struct {
	Message string
	// RetryAfter is meridian's retry_after in seconds, 0 when the body had none.
	RetryAfter int
}

// ParseLimit recognises a failure error object as a usage limit.
func ParseLimit(raw json.RawMessage) (Limit, bool) {
	if len(raw) == 0 {
		return Limit{}, false
	}
	var e struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Response struct {
			Body string `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Type != "provider.rate-limit" {
		return Limit{}, false
	}
	l := Limit{Message: e.Message}
	var body struct {
		Error struct {
			RetryAfter int `json:"retry_after"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.Response.Body), &body) == nil && body.Error.RetryAfter > 0 {
		l.RetryAfter = body.Error.RetryAfter
	}
	return l, true
}
