package opencode

import "encoding/json"

// Limit is a usage-limit failure as OpenCode reports it for meridian's rate-limit error.
type Limit struct {
	Message string
	// RetryAfter is meridian's retry_after in seconds, 0 when the body had none.
	RetryAfter int
}

// LimitStop is a session whose newest real message is a usage-limit failure.
type LimitStop struct {
	MessageID string // the failed assistant message: while it is the newest real message, nobody went on
	At        int64  // when the session stopped (epoch ms)
	Limit     Limit
}

// StopOf reads a session's newest messages (newest first) and reports whether it stands at a limit stop.
// Idle entries are skipped: OpenCode sometimes runs an empty execution right after a limit failure (no
// model call, no message, an "idle succeeded" 5–60 ms later; measured 2026-11-14), which must not hide the
// stop. Any other message newer than the failed one means someone went on.
func StopOf(newestFirst []Message) (LimitStop, bool) {
	var failedAt int64
	for _, m := range newestFirst {
		if m.Type == "idle" {
			if m.Outcome == "failed" {
				failedAt = m.Time.Created
			}
			continue
		}
		if m.Type != "assistant" {
			return LimitStop{}, false
		}
		l, ok := ParseLimit(m.Error)
		if !ok {
			return LimitStop{}, false
		}
		if failedAt == 0 {
			failedAt = m.Time.Created
		}
		return LimitStop{MessageID: m.ID, At: failedAt, Limit: l}, true
	}
	return LimitStop{}, false
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
