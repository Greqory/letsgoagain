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

// rejectedFloor is the utilization below which a "rejected" status is taken as stale. meridian keeps
// the status after the window reset, and the next window then carries a new, future resetsAt: measured
// 2026-11-15, five_hour rejected at 37 % with a reset 5 h ahead, which kept a session waiting for hours
// with no limit hit.
const rejectedFloor = 0.95

// Spent reports whether the window is used up: fully used, or meridian says rejected and the
// utilization does not contradict it. Whether it has reset since is up to the caller (resetsAt).
func (b Bucket) Spent() bool {
	rejected := b.Status == "rejected" && (b.Utilization == nil || *b.Utilization >= rejectedFloor)
	return rejected || (b.Utilization != nil && *b.Utilization >= 1)
}

// Exhausted returns the latest reset among the windows that are spent and reset after now. A past
// resetsAt means the window has reset, whatever the (sticky) status says.
func (q Quota) Exhausted(now time.Time) (resetAt time.Time, window string, ok bool) {
	for _, b := range q.Buckets {
		if !b.Spent() || b.ResetsAt == nil {
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

// Version reads meridian's version from /health. Only that field is decoded: /health also carries the
// account's email, which LetsGoAgain has no business with.
func (c *Client) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("version: status %d", resp.StatusCode)
	}
	var h struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return "", fmt.Errorf("version: decode: %w", err)
	}
	return h.Version, nil
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
