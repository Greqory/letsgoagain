package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned when the service does not know the session.
var ErrNotFound = errors.New("not found")

// Session is the part of the API's Session.Info that LetsGoAgain reads.
type Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Title    string `json:"title"`
	Outcome  string `json:"outcome"`
	Time     struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
		Busy    int64 `json:"busy"`
		Idle    int64 `json:"idle"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// Message is the part of a session message that LetsGoAgain reads.
type Message struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Outcome string          `json:"outcome"`
	Error   json.RawMessage `json:"error"`
	Time    struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// Client calls one service. Its zero HTTP client is replaced by one with a timeout.
type Client struct {
	base     string
	password string
	http     *http.Client
}

// NewClient builds a client for a service.
func NewClient(s Service) *Client {
	return &Client{base: strings.TrimRight(s.URL, "/"), password: s.Password, http: &http.Client{Timeout: 30 * time.Second}}
}

// Session returns one session.
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	var out struct {
		Data Session `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(id), nil, &out)
	return out.Data, err
}

// RootSessions returns the newest root sessions, newest first.
func (c *Client) RootSessions(ctx context.Context, limit int) ([]Session, error) {
	var out struct {
		Data []Session `json:"data"`
	}
	q := url.Values{"parentID": {"null"}, "order": {"desc"}, "limit": {fmt.Sprint(limit)}}
	err := c.do(ctx, http.MethodGet, "/api/session?"+q.Encode(), nil, &out)
	return out.Data, err
}

// LatestMessage returns the newest message of a session.
func (c *Client) LatestMessage(ctx context.Context, id string) (Message, error) {
	var out struct {
		Data []Message `json:"data"`
	}
	q := url.Values{"limit": {"1"}, "order": {"desc"}}
	if err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(id)+"/message?"+q.Encode(), nil, &out); err != nil {
		return Message{}, err
	}
	if len(out.Data) == 0 {
		return Message{}, fmt.Errorf("session %s: no messages", id)
	}
	return out.Data[0], nil
}

// Running reports whether a session is executing right now.
func (c *Client) Running(ctx context.Context, id string) (bool, error) {
	var out struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/active", nil, &out); err != nil {
		return false, err
	}
	_, ok := out.Data[id]
	return ok, nil
}

// Prompt sends one user message to a session.
func (c *Client) Prompt(ctx context.Context, id, text string) error {
	return c.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(id)+"/prompt", map[string]string{"text": text}, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s %s: encode: %w", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := c.request(ctx, method, path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, pathOnly(path), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, pathOnly(path), ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s %s: status %d: %s", method, pathOnly(path), resp.StatusCode, bytes.TrimSpace(snippet))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, pathOnly(path), err)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, pathOnly(path), err)
	}
	req.SetBasicAuth("opencode", c.password)
	return req, nil
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
