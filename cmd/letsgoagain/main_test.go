package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Greqory/letsgoagain/internal/state"
)

func TestUnitFile(t *testing.T) {
	u := unitFile("/home/greg/go/bin/letsgoagain", true)
	if !strings.Contains(u, "ExecStart=/home/greg/go/bin/letsgoagain daemon --dry-run\n") || !strings.Contains(u, "WantedBy=default.target") {
		t.Fatalf("unit:\n%s", u)
	}
	if u := unitFile("/home/greg/my bin/letsgoagain", false); !strings.Contains(u, `ExecStart="/home/greg/my bin/letsgoagain" daemon`+"\n") {
		t.Fatalf("quoted unit:\n%s", u)
	}
}

func TestUntil(t *testing.T) {
	now := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{-time.Minute: "due", 7 * time.Minute: "7m", 81 * time.Minute: "1h 21m", 49 * time.Hour: "2d 1h"} {
		if got := until(now.Add(d), now); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
}

func TestPrintStops(t *testing.T) {
	now := time.Now()
	var buf bytes.Buffer
	printStops(&buf, nil, now)
	if buf.String() != "no stops tracked\n" {
		t.Fatalf("empty: %q", buf.String())
	}
	buf.Reset()
	printStops(&buf, []state.Stop{
		{SessionID: "ses_a", Title: "F003-T120: a long task title that goes on and on", Status: state.Waiting, NextAt: now.Add(81 * time.Minute), Source: "quota", Window: "five_hour"},
		{SessionID: "ses_b", Status: state.Done, Attempts: 1},
	}, now)
	out := buf.String()
	for _, want := range []string{"STATUS", "waiting", "F003-T120: a long task title that goes … ses_a", "(in 1h 21m)", "quota five_hour", "0/5", "done", "1/5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00:53") || strings.Contains(out, "Mon 00:") {
		t.Errorf("a zero stop time is printed as a date:\n%s", out)
	}
}
