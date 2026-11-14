package meridian

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func f(v float64) *float64  { return &v }
func ms(t time.Time) *int64 { v := t.UnixMilli(); return &v }

func TestExhausted(t *testing.T) {
	now := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	in5h := now.Add(81 * time.Minute)
	in2d := now.Add(49 * time.Hour)
	tests := []struct {
		name    string
		buckets []Bucket
		want    time.Time
		window  string
	}{
		{"none spent", []Bucket{{Type: "five_hour", Status: "allowed", Utilization: f(0.26), ResetsAt: ms(in5h)}}, time.Time{}, ""},
		{"5h rejected", []Bucket{{Type: "five_hour", Status: "rejected", Utilization: f(1), ResetsAt: ms(in5h)}, {Type: "seven_day", Status: "allowed", Utilization: f(0.53), ResetsAt: ms(in2d)}}, in5h, "five_hour"},
		{"full but status lags", []Bucket{{Type: "five_hour", Status: "allowed_warning", Utilization: f(1), ResetsAt: ms(in5h)}}, in5h, "five_hour"},
		{"both spent, weekly wins", []Bucket{{Type: "five_hour", Status: "rejected", ResetsAt: ms(in5h)}, {Type: "seven_day", Status: "rejected", ResetsAt: ms(in2d)}}, in2d, "seven_day"},
		{"sticky rejected after reset", []Bucket{{Type: "five_hour", Status: "rejected", Utilization: f(1), ResetsAt: ms(now.Add(-time.Minute))}}, time.Time{}, ""},
		{"rejected without reset", []Bucket{{Type: "five_hour", Status: "rejected"}}, time.Time{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, window, ok := Quota{Buckets: tt.buckets}.Exhausted(now)
			if ok != !tt.want.IsZero() || !at.Equal(tt.want) || window != tt.window {
				t.Fatalf("got %v %q %v; want %v %q", at, window, ok, tt.want, tt.window)
			}
		})
	}
}

func TestClientMeasuredQuota(t *testing.T) {
	body, err := os.ReadFile("testdata/quota_allowed.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage/quota" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	q, err := NewClient(srv.URL + "/").Quota(context.Background())
	if err != nil || len(q.Buckets) != 3 || q.Buckets[0].Type != "five_hour" || *q.Buckets[0].ResetsAt != 1794623400461 {
		t.Fatalf("got %+v, %v", q, err)
	}
	if _, _, ok := q.Exhausted(time.UnixMilli(1794608470638)); ok {
		t.Fatal("measured quota has no spent window")
	}
	if _, err := NewClient(srv.URL + "/nope").Quota(context.Background()); err == nil {
		t.Fatal("want an error for a 404")
	}
}

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The shape meridian 1.79.0 answers with, account details shortened.
		w.Write([]byte(`{"status":"healthy","version":"1.79.0","backend":"claude","build":{"source":"npm","version":"1.79.0","latest":"1.89.0","updateAvailable":true},"auth":{"loggedIn":true,"email":"someone@example.com"}}`))
	}))
	defer srv.Close()
	v, err := NewClient(srv.URL).Version(context.Background())
	if err != nil || v != "1.79.0" {
		t.Fatalf("got %q, %v", v, err)
	}
	if _, err := NewClient(srv.URL + "/nope").Version(context.Background()); err == nil {
		t.Fatal("want an error for a 404")
	}
}
