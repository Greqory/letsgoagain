package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Event types LetsGoAgain follows.
const (
	EventExecutionStarted   = "session.execution.started"
	EventExecutionSucceeded = "session.execution.succeeded"
	EventExecutionFailed    = "session.execution.failed"
)

// Event is one event of the service's stream.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		SessionID string          `json:"sessionID"`
		Error     json.RawMessage `json:"error"`
	} `json:"data"`
}

// Events subscribes to the given event types and calls handle for each until the stream ends, ctx is
// cancelled or handle returns an error. connected is called once the service accepted the subscription.
func (c *Client) Events(ctx context.Context, types []string, connected func(), handle func(Event) error) error {
	q := url.Values{"types": types}
	req, err := c.request(ctx, http.MethodGet, "/api/event?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The client's timeout would cut the stream; the context ends it instead.
	resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
	if err != nil {
		return fmt.Errorf("event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("event stream: status %d", resp.StatusCode)
	}
	if connected != nil {
		connected()
	}
	return readSSE(resp, handle)
}

func readSSE(resp *http.Response, handle func(Event) error) error {
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			if data.Len() == 0 {
				continue
			}
			var ev Event
			err := json.Unmarshal([]byte(data.String()), &ev)
			data.Reset()
			if err != nil {
				continue // a frame we cannot read is not ours to act on
			}
			if err := handle(ev); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("event stream: %w", err)
	}
	return fmt.Errorf("event stream: closed by the service")
}
