// Package meridian reads the meridian proxy's account quota (/v1/usage/quota) and tells which limit
// window is exhausted and when it resets.
package meridian

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is where opencode-with-claude starts meridian.
const DefaultURL = "http://127.0.0.1:3456"

// Bucket is one limit window.
type Bucket struct {
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Utilization *float64 `json:"utilization"`
	ResetsAt    *int64   `json:"resetsAt"`
}

// Quota is meridian's view of the account.
type Quota struct {
	Buckets []Bucket `json:"buckets"`
}

// Exhausted returns the latest reset among the windows that are spent and reset after now. A window
// counts as spent when meridian says rejected or it is fully used; a past resetsAt means it has reset,
// whatever the (sticky) status says.
func (q Quota) Exhausted(now time.Time) (resetAt time.Time, window string, ok bool) {
	for _, b := range q.Buckets {
		spent := b.Status == "rejected" || (b.Utilization != nil && *b.Utilization >= 1)
		if !spent || b.ResetsAt == nil {
			continue
		}
		at := time.UnixMilli(*b.ResetsAt)
		if at.After(now) && at.After(resetAt) {
			resetAt, window, ok = at, b.Type, true
		}
	}
	return resetAt, window, ok
}

// Client reads one meridian.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds a client for meridian at base.
func NewClient(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 15 * time.Second}}
}

// Quota fetches the current quota.
func (c *Client) Quota(ctx context.Context) (Quota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/usage/quota", nil)
	if err != nil {
		return Quota{}, fmt.Errorf("quota: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Quota{}, fmt.Errorf("quota: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Quota{}, fmt.Errorf("quota: status %d", resp.StatusCode)
	}
	var q Quota
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return Quota{}, fmt.Errorf("quota: decode: %w", err)
	}
	return q, nil
}
