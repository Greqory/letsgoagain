package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Greqory/letsgoagain/internal/daemon"
	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/state"
)

// recentWindow is how far back closed stops are shown unless --all is given.
const recentWindow = 24 * time.Hour

func runStatus(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	all := fs.Bool("all", false, "every tracked stop, with full dates (default: open ones and those of the last 24 hours)")
	ampm := fs.Bool("12h", false, "show times on a 12-hour clock (AM/PM)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ov, running, err := loadOverview()
	if err != nil {
		return err
	}
	printStatus(w, ov, running, clock{ampm: *ampm}, *all, time.Now())
	return nil
}

// loadOverview asks the daemon, falling back to the state file when it is not running.
func loadOverview() (daemon.Overview, bool, error) {
	if socket, err := daemon.DefaultSocketPath(); err == nil {
		if ov, err := daemon.NewControl(socket).Overview(); err == nil {
			return ov, true, nil
		}
	}
	dir, err := stateDir()
	if err != nil {
		return daemon.Overview{}, false, err
	}
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		return daemon.Overview{}, false, err
	}
	var ov daemon.Overview
	for _, s := range st.Sorted() {
		ov.Stops = append(ov.Stops, *s)
	}
	return ov, false, nil
}

// clock formats times for the terminal: 24-hour unless asked otherwise. The locale is not consulted: a
// LANG of en_US says little about the clock someone reads (the author's own is en_US with a 24-hour day).
type clock struct{ ampm bool }

// at formats t as weekday and time; the date joins them when asked for, or when t is a day or more
// away from now and the weekday alone would be ambiguous.
func (c clock) at(t, now time.Time, withDate bool) string {
	day, hm := "Mon ", "15:04"
	if withDate || t.Sub(now).Abs() >= 24*time.Hour {
		day = "Mon 2 Jan "
	}
	if c.ampm {
		hm = "3:04 PM"
	}
	return t.Local().Format(day + hm)
}

func printStatus(w io.Writer, ov daemon.Overview, running bool, c clock, all bool, now time.Time) {
	if !running {
		fmt.Fprintln(w, "daemon: not running (showing the state file)")
		printStops(w, ov.Stops, c, all, now)
		return
	}
	fmt.Fprintln(w, daemonLine(ov))
	fmt.Fprintln(w)
	printQuota(w, ov, c, now)
	fmt.Fprintln(w)
	printListening(w, ov, now)
	fmt.Fprintln(w)
	printStops(w, ov.Stops, c, all, now)
}

func daemonLine(ov daemon.Overview) string {
	line := "daemon: running, "
	if ov.Connected {
		line += "connected to OpenCode"
		if ov.OpenCode != "" {
			line += " " + ov.OpenCode
		}
	} else {
		line += "OpenCode service not reachable (retrying)"
	}
	if ov.DryRun {
		line += ", dry run: nothing is resumed"
	}
	return line
}

// quotaOrder puts the known windows first, in the order of how soon they matter.
var quotaOrder = map[string]int{"five_hour": 0, "seven_day": 1, "seven_day_fable": 2}

var quotaNames = map[string]string{"five_hour": "5-hour", "seven_day": "weekly", "seven_day_fable": "weekly Fable"}

func printQuota(w io.Writer, ov daemon.Overview, c clock, now time.Time) {
	fmt.Fprintln(w, "Quota")
	if ov.Quota == nil {
		fmt.Fprintf(w, "  not available: %s\n", ov.QuotaErr)
		return
	}
	buckets := append([]meridian.Bucket(nil), ov.Quota.Buckets...)
	rank := func(b meridian.Bucket) int {
		if r, ok := quotaOrder[b.Type]; ok {
			return r
		}
		return len(quotaOrder)
	}
	sort.SliceStable(buckets, func(i, j int) bool { return rank(buckets[i]) < rank(buckets[j]) })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, b := range buckets {
		name := quotaNames[b.Type]
		if name == "" {
			name = b.Type
		}
		used, reset, note := "-", "-", ""
		over := b.ResetsAt != nil && !time.UnixMilli(*b.ResetsAt).After(now)
		if over {
			// The window is over; its figures are from before and meridian has no new ones yet.
			reset = "reset passed"
		} else {
			if b.Utilization != nil {
				used = fmt.Sprintf("%.0f%%", *b.Utilization*100)
			}
			if b.ResetsAt != nil {
				at := time.UnixMilli(*b.ResetsAt)
				reset = fmt.Sprintf("resets %s (in %s)", c.at(at, now, false), until(at, now))
			}
			switch {
			case b.Spent():
				note = "LIMIT REACHED"
			case b.Status == "allowed_warning":
				note = "warning"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, used, reset, note)
	}
	tw.Flush()
}

func printListening(w io.Writer, ov daemon.Overview, now time.Time) {
	switch {
	case !ov.Connected:
		fmt.Fprintln(w, "Listening: not connected to the OpenCode service, nothing is watched right now")
		return
	case ov.ListeningErr != "":
		fmt.Fprintf(w, "Listening: running sessions not readable: %s\n", ov.ListeningErr)
		return
	case len(ov.Listening) == 0:
		fmt.Fprintln(w, "Listening: no session is running right now (any that hits the limit later is still caught)")
		return
	}
	fmt.Fprintf(w, "Listening: %d running; one that hits the limit continues after the reset\n", len(ov.Listening))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, l := range ov.Listening {
		since := ""
		if !l.Since.IsZero() {
			since = "running " + span(now.Sub(l.Since))
		}
		fmt.Fprintf(tw, "  %s\t%s\n", labelOf(l.Title, l.SessionID), since)
	}
	tw.Flush()
}

// recent keeps the open stops and the closed ones that changed within recentWindow.
func recent(stops []state.Stop, now time.Time) []state.Stop {
	var out []state.Stop
	for _, s := range stops {
		if s.Open() || now.Sub(s.Touched()) < recentWindow {
			out = append(out, s)
		}
	}
	return out
}

func printStops(w io.Writer, stops []state.Stop, c clock, all bool, now time.Time) {
	shown := stops
	if !all {
		shown = recent(stops, now)
	}
	switch {
	case len(stops) == 0:
		fmt.Fprintln(w, "Stops: none tracked")
		return
	case all:
		fmt.Fprintf(w, "Stops: all %d tracked (closed ones are kept for 7 days)\n", len(stops))
	case len(shown) == len(stops):
		fmt.Fprintln(w, "Stops: open ones and the last 24 hours")
	case len(shown) == 0:
		fmt.Fprintf(w, "Stops: none open or in the last 24 hours (--all shows the %d older)\n", len(stops))
		return
	default:
		fmt.Fprintf(w, "Stops: open ones and the last 24 hours (--all shows the %d older)\n", len(stops)-len(shown))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tSESSION\tSTOPPED\tNEXT\tFROM\tRESUMES\tNOTE")
	for _, s := range shown {
		next := "-"
		if s.Open() && s.Status == state.Waiting {
			next = fmt.Sprintf("%s (in %s)", c.at(s.NextAt, now, all), until(s.NextAt, now))
		}
		from := s.Source
		if s.Window != "" {
			from += " " + s.Window
		}
		stopped := "-"
		if !s.StoppedAt.IsZero() {
			stopped = c.at(s.StoppedAt, now, all)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n", s.Status, labelOf(s.Title, s.SessionID), stopped,
			next, from, s.Attempts, state.MaxAttempts, s.Note)
	}
	tw.Flush()
}

func labelOf(title, id string) string {
	name := title
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:39]) + "…"
	}
	if name == "" {
		return id
	}
	return name + " " + id
}

func until(t, now time.Time) string {
	if t.Sub(now).Round(time.Minute) <= 0 {
		return "due"
	}
	return span(t.Sub(now))
}

// span writes a duration as "7m", "1h 21m" or "2d 1h".
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h >= 24 {
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
