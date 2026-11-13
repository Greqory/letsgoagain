// Package fakemeridian is a stand-in for the meridian proxy (@rynfar/meridian 1.79.0) in tests and
// sandbox runs: an Anthropic-style /messages stream that either answers or fails with meridian's exact
// rate-limit error frame (a 200 response carrying an SSE `error` event), and /v1/usage/quota.
package fakemeridian

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Bucket mirrors one entry of meridian's /v1/usage/quota `buckets`.
type Bucket struct {
	Type        string  `json:"type"`
	Status      string  `json:"status"`
	Utilization float64 `json:"utilization"`
	ResetsAt    int64   `json:"resetsAt"`
	ObservedAt  int64   `json:"observedAt"`
}

// Server is an http.Handler; its zero value answers normally with an empty quota.
type Server struct {
	mu         sync.Mutex
	limited    bool
	retryAfter int
	buckets    []Bucket
	requests   int
}

// SetLimited switches the /messages answer to meridian's rate-limit frame (retryAfter seconds, 0 = no
// retry_after field) or back to a normal answer.
func (s *Server) SetLimited(limited bool, retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limited, s.retryAfter = limited, retryAfter
}

// SetBuckets replaces the quota buckets.
func (s *Server) SetBuckets(b []Bucket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets = b
}

// Requests counts the /messages requests served.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/messages" || r.URL.Path == "/v1/messages"):
		s.messages(w)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/usage/quota":
		s.quota(w)
	case r.Method == http.MethodPost && r.URL.Path == "/fake/limit":
		var req struct {
			Limited    bool `json:"limited"`
			RetryAfter int  `json:"retry_after"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetLimited(req.Limited, req.RetryAfter)
	case r.Method == http.MethodPost && r.URL.Path == "/fake/buckets":
		var b []Bucket
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetBuckets(b)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) messages(w http.ResponseWriter) {
	s.mu.Lock()
	s.requests++
	limited, retryAfter := s.limited, s.retryAfter
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if limited {
		e := map[string]any{"type": "rate_limit_error", "message": "Claude Max rate limit reached. Wait a moment and try again."}
		if retryAfter > 0 {
			e["retry_after"] = retryAfter
		}
		event(w, "error", map[string]any{"type": "error", "error": e})
		return
	}
	id := fmt.Sprintf("msg_fake%d", time.Now().UnixNano())
	event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": "claude-fake", "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0},
	}})
	event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "ok"}})
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event(w, "message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *Server) quota(w http.ResponseWriter) {
	s.mu.Lock()
	buckets := append([]Bucket{}, s.buckets...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"profile": nil, "buckets": buckets, "asOf": time.Now().UnixMilli()})
}

func event(w http.ResponseWriter, name string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
