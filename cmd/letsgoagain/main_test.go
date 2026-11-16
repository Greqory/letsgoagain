package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Greqory/letsgoagain/internal/daemon"
	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/state"
)

func TestUnitFile(t *testing.T) {
	u := unitFile("/home/greg/go/bin/letsgoagain", "--dry-run")
	if !strings.Contains(u, "ExecStart=/home/greg/go/bin/letsgoagain daemon --dry-run\n") || !strings.Contains(u, "WantedBy=default.target") {
		t.Fatalf("unit:\n%s", u)
	}
	if u := unitFile("/home/greg/my bin/letsgoagain"); !strings.Contains(u, `ExecStart="/home/greg/my bin/letsgoagain" daemon`+"\n") {
		t.Fatalf("quoted unit:\n%s", u)
	}
	if u := unitFile("/opt/letsgoagain", "--dry-run", "--no-notice"); !strings.Contains(u, "ExecStart=/opt/letsgoagain daemon --dry-run --no-notice\n") {
		t.Fatalf("flags:\n%s", u)
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

var statusNow = time.Date(2026, 11, 16, 8, 0, 0, 0, time.Local) // a Monday

// squash collapses runs of spaces, so the checks don't depend on the column widths.
func squash(s string) string { return regexp.MustCompile(` +`).ReplaceAllString(s, " ") }

func pct(f float64) *float64 { return &f }

func resetsIn(d time.Duration) *int64 {
	ms := statusNow.Add(d).UnixMilli()
	return &ms
}

func sampleOverview() daemon.Overview {
	return daemon.Overview{
		Connected: true,
		OpenCode:  "2.3.0",
		Quota: &meridian.Quota{Buckets: []meridian.Bucket{
			{Type: "seven_day_fable", Status: "allowed", Utilization: pct(0), ResetsAt: resetsIn(7*24*time.Hour - time.Hour)},
			{Type: "seven_day", Status: "rejected", Utilization: pct(0.03), ResetsAt: resetsIn(6*24*time.Hour + 23*time.Hour)}, // sticky status
			{Type: "five_hour", Status: "allowed_warning", Utilization: pct(0.06), ResetsAt: resetsIn(5*time.Hour + 3*time.Minute)},
		}},
		Listening: []daemon.Listener{
			{SessionID: "ses_run", Title: "F017 coordination", Since: statusNow.Add(-90 * time.Minute)},
			{SessionID: "ses_new"},
		},
		Stops: []state.Stop{
			{SessionID: "ses_wait", Title: "waiting one", Status: state.Waiting, Source: "quota", Window: "five_hour", NextAt: statusNow.Add(81 * time.Minute),
				StoppedAt: statusNow.Add(-time.Hour), UpdatedAt: statusNow.Add(-time.Hour)},
			{SessionID: "ses_fresh", Title: "closed last night", Status: state.Done, Attempts: 1,
				StoppedAt: time.Date(2026, 11, 15, 23, 2, 0, 0, time.Local), UpdatedAt: statusNow.Add(-8 * time.Hour)},
			{SessionID: "ses_old", Title: "closed long ago", Status: state.Done, Attempts: 1,
				StoppedAt: time.Date(2026, 11, 14, 14, 11, 0, 0, time.Local), UpdatedAt: statusNow.Add(-42 * time.Hour)},
		},
	}
}

func TestPrintStatusDefault(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, sampleOverview(), true, clock{}, false, statusNow)
	out := squash(buf.String())
	for _, want := range []string{
		"daemon: running, connected to OpenCode 2.3.0\n",
		"5-hour 6% resets Mon 13:03 (in 5h 03m) warning",
		"weekly 3% resets Mon 23 Nov 07:00 (in 6d 23h)", // a week out: the weekday alone would mislead
		"weekly Fable 0% resets Mon 23 Nov 07:00",
		"Listening: 2 running;",
		"F017 coordination ses_run running 1h 30m",
		"Stops: open ones and the last 24 hours (--all shows the 1 older)",
		"waiting waiting one ses_wait Mon 07:00 Mon 09:21 (in 1h 21m) quota five_hour 0/5",
		"closed last night ses_fresh Sun 23:02",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"closed long ago", "LIMIT REACHED", "AM", "PM"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output has %q:\n%s", unwanted, out)
		}
	}
	if q, l, s := strings.Index(out, "Quota"), strings.Index(out, "Listening"), strings.Index(out, "Stops"); !(q < l && l < s) {
		t.Errorf("sections out of order:\n%s", out)
	}
	if w, f := strings.Index(out, "ses_wait"), strings.Index(out, "ses_fresh"); w > f {
		t.Errorf("open stop below a closed one:\n%s", out)
	}
}

func TestPrintStatusAll(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, sampleOverview(), true, clock{}, true, statusNow)
	out := squash(buf.String())
	for _, want := range []string{
		"Stops: all 3 tracked",
		"waiting waiting one ses_wait Mon 16 Nov 07:00 Mon 16 Nov 09:21 (in 1h 21m)",
		"closed last night ses_fresh Sun 15 Nov 23:02",
		"closed long ago ses_old Sat 14 Nov 14:11",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if f, o := strings.Index(out, "ses_fresh"), strings.Index(out, "ses_old"); f > o {
		t.Errorf("older stop above a newer one:\n%s", out)
	}
}

func TestPrintStatus12h(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, sampleOverview(), true, clock{ampm: true}, false, statusNow)
	out := squash(buf.String())
	for _, want := range []string{"resets Mon 1:03 PM (in 5h 03m)", "Sun 11:02 PM", "Mon 9:21 AM (in 1h 21m)", "Mon 23 Nov 7:00 AM"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "13:03") {
		t.Errorf("24-hour time in a 12-hour view:\n%s", out)
	}
}

func TestPrintStatusStates(t *testing.T) {
	status := func(mutate func(*daemon.Overview), running bool) string {
		ov := sampleOverview()
		mutate(&ov)
		var buf bytes.Buffer
		printStatus(&buf, ov, running, clock{}, false, statusNow)
		return squash(buf.String())
	}
	for name, tc := range map[string]struct {
		mutate  func(*daemon.Overview)
		running bool
		want    []string
		not     []string
	}{
		"dry run": {func(o *daemon.Overview) { o.DryRun = true }, true, []string{"connected to OpenCode 2.3.0, dry run: nothing is resumed"}, nil},
		"service down": {func(o *daemon.Overview) { o.Connected, o.OpenCode, o.Listening = false, "", nil }, true,
			[]string{"daemon: running, OpenCode service not reachable (retrying)", "Listening: not connected"}, nil},
		"nothing running": {func(o *daemon.Overview) { o.Listening = nil }, true, []string{"Listening: no session is running right now"}, nil},
		"running list unreadable": {func(o *daemon.Overview) { o.Listening, o.ListeningErr = nil, "boom" }, true,
			[]string{"Listening: running sessions not readable: boom"}, []string{"no session is running"}},
		"quota down": {func(o *daemon.Overview) { o.Quota, o.QuotaErr = nil, "quota: connection refused" }, true,
			[]string{"Quota\n not available: quota: connection refused"}, nil},
		"limit reached":  {func(o *daemon.Overview) { o.Quota.Buckets[2].Utilization = pct(1) }, true, []string{"100%", "LIMIT REACHED"}, nil},
		"window over":    {func(o *daemon.Overview) { o.Quota.Buckets[2].ResetsAt = resetsIn(-time.Minute) }, true, []string{"5-hour - reset passed"}, []string{"LIMIT", "6%"}},
		"daemon off":     {func(o *daemon.Overview) {}, false, []string{"daemon: not running (showing the state file)", "Stops:", "ses_wait"}, []string{"Quota", "Listening"}},
		"no stops":       {func(o *daemon.Overview) { o.Stops = nil }, true, []string{"Stops: none tracked"}, []string{"STATUS"}},
		"only old stops": {func(o *daemon.Overview) { o.Stops = o.Stops[2:] }, true, []string{"Stops: none open or in the last 24 hours (--all shows the 1 older)"}, []string{"STATUS"}},
	} {
		out := status(tc.mutate, tc.running)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: output lacks %q:\n%s", name, want, out)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(out, not) {
				t.Errorf("%s: output has %q:\n%s", name, not, out)
			}
		}
	}
}
