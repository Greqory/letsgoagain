package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseLimit(t *testing.T) {
	var failed Event
	if err := json.Unmarshal(fixture(t, "event_execution_failed_ratelimit.json"), &failed); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		raw        string
		want       bool
		retryAfter int
	}{
		{"measured execution.failed", string(failed.Data.Error), true, 4868},
		{"no retry_after", `{"type":"provider.rate-limit","message":"m","response":{"body":"{\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\"}}"}}`, true, 0},
		{"body not json", `{"type":"provider.rate-limit","message":"m","response":{"body":"oops"}}`, true, 0},
		{"aborted", `{"type":"aborted","message":"Step interrupted"}`, false, 0},
		{"internal", `{"type":"provider.internal","message":"api_error: Reached maximum number of turns (1)"}`, false, 0},
		{"empty", ``, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok := ParseLimit(json.RawMessage(tt.raw))
			if ok != tt.want || l.RetryAfter != tt.retryAfter {
				t.Fatalf("got %+v, %v; want ok=%v retryAfter=%d", l, ok, tt.want, tt.retryAfter)
			}
		})
	}
}

func TestReadService(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "service.json")
	if err := os.WriteFile(good, []byte(`{"id":"x","version":"2.3.0","url":"http://127.0.0.1:1","pid":7,"password":"secret-pw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadService(good)
	if err != nil || s.URL != "http://127.0.0.1:1" || s.Password != "secret-pw" || s.PID != 7 {
		t.Fatalf("got %+v, %v", s, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"url":"http://x","password":"secret-pw"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadService(bad); err == nil || strings.Contains(err.Error(), "secret-pw") {
		t.Fatalf("want an error without the password, got %v", err)
	}
	if _, err := ReadService(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func fakeService(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewClient(Service{URL: srv.URL, Password: "pw"})
}

func TestClient(t *testing.T) {
	var prompted string
	c := fakeService(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/session/ses_e28fa4af7ffeqz7CiHBYs0EJ3B" && r.Method == http.MethodGet:
			w.Write(fixture(t, "session_failed.json"))
		case r.URL.Path == "/api/session/ses_e28fa4af7ffeqz7CiHBYs0EJ3B/message":
			if r.URL.Query().Get("order") != "desc" || r.URL.Query().Get("limit") != "1" {
				t.Errorf("message query %q", r.URL.RawQuery)
			}
			w.Write(fixture(t, "messages_failed_desc.json"))
		case r.URL.Path == "/api/session/ses_e28fa4af7ffeqz7CiHBYs0EJ3B/prompt" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			prompted = string(b)
			w.Write([]byte(`{"data":{}}`))
		case r.URL.Path == "/api/session/active":
			w.Write([]byte(`{"data":{"ses_busy":{"type":"running"}}}`))
		case r.URL.Path == "/api/session" && r.Method == http.MethodGet:
			if r.URL.Query().Get("parentID") != "null" {
				t.Errorf("roots query %q", r.URL.RawQuery)
			}
			w.Write([]byte(`{"data":[{"id":"ses_a","outcome":"failed"},{"id":"ses_b"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	const id = "ses_e28fa4af7ffeqz7CiHBYs0EJ3B"

	s, err := c.Session(ctx, id)
	if err != nil || s.Outcome != "failed" || s.Time.Idle != 1794608917050 || s.ParentID != "" || s.Location.Directory != "/tmp/opencode/lga-sandbox/work" {
		t.Fatalf("session: %+v, %v", s, err)
	}
	m, err := c.LatestMessage(ctx, id)
	if err != nil || m.Type != "idle" || m.Outcome != "failed" {
		t.Fatalf("latest message: %+v, %v", m, err)
	}
	if l, ok := ParseLimit(m.Error); !ok || l.RetryAfter != 4868 {
		t.Fatalf("latest message limit: %+v, %v", l, ok)
	}
	if err := c.Prompt(ctx, id, "Continue where you left off."); err != nil || prompted != `{"text":"Continue where you left off."}` {
		t.Fatalf("prompt: %q, %v", prompted, err)
	}
	if busy, err := c.Running(ctx, "ses_busy"); err != nil || !busy {
		t.Fatalf("running busy: %v, %v", busy, err)
	}
	if busy, err := c.Running(ctx, id); err != nil || busy {
		t.Fatalf("running idle: %v, %v", busy, err)
	}
	roots, err := c.RootSessions(ctx, 50)
	if err != nil || len(roots) != 2 || roots[0].Outcome != "failed" {
		t.Fatalf("roots: %+v, %v", roots, err)
	}
	if _, err := c.Session(ctx, "ses_gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	wrong := NewClient(Service{URL: c.base, Password: "nope"})
	if _, err := wrong.Session(ctx, id); err == nil || strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestEvents(t *testing.T) {
	failed := fixture(t, "event_execution_failed_ratelimit.json")
	var compact bytes.Buffer
	if err := json.Compact(&compact, failed); err != nil {
		t.Fatal(err)
	}
	c := fakeService(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["types"]; len(got) != 2 || got[0] != EventExecutionFailed {
			t.Errorf("types %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: server.connected\ndata: {\"type\":\"server.connected\"}\n\n")
		fmt.Fprint(w, ": keep-alive\n\ndata: not json\n\n")
		fmt.Fprintf(w, "id: 1\nevent: %s\ndata: %s\n\n", EventExecutionFailed, compact.String())
	})
	var got []Event
	connected := false
	err := c.Events(context.Background(), []string{EventExecutionFailed, EventExecutionSucceeded}, func() { connected = true }, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("want a closed-stream error, got %v", err)
	}
	if !connected || len(got) != 2 || got[1].Type != EventExecutionFailed || got[1].Data.SessionID != "ses_e28fa4af7ffeqz7CiHBYs0EJ3B" {
		t.Fatalf("connected=%v events=%+v", connected, got)
	}
	if _, ok := ParseLimit(got[1].Data.Error); !ok {
		t.Fatal("limit not recognised in the streamed event")
	}
}
