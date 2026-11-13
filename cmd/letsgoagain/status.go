package main

import (
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/Greqory/letsgoagain/internal/daemon"
	"github.com/Greqory/letsgoagain/internal/state"
)

func runStatus(w io.Writer) error {
	stops, running, err := loadStops()
	if err != nil {
		return err
	}
	if !running {
		fmt.Fprintln(w, "daemon: not running (showing the state file)")
	}
	printStops(w, stops, time.Now())
	return nil
}

// loadStops asks the daemon, falling back to the state file when it is not running.
func loadStops() ([]state.Stop, bool, error) {
	if socket, err := daemon.DefaultSocketPath(); err == nil {
		if stops, err := daemon.NewControl(socket).Status(); err == nil {
			return stops, true, nil
		}
	}
	dir, err := stateDir()
	if err != nil {
		return nil, false, err
	}
	st, err := state.Load(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, false, err
	}
	var stops []state.Stop
	for _, s := range st.Sorted() {
		stops = append(stops, *s)
	}
	return stops, false, nil
}

func printStops(w io.Writer, stops []state.Stop, now time.Time) {
	if len(stops) == 0 {
		fmt.Fprintln(w, "no stops tracked")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tSESSION\tSTOPPED\tNEXT\tFROM\tRESUMES\tNOTE")
	for _, s := range stops {
		next := "-"
		if s.Open() && s.Status == state.Waiting {
			next = fmt.Sprintf("%s (in %s)", s.NextAt.Local().Format("Mon 15:04"), until(s.NextAt, now))
		}
		from := s.Source
		if s.Window != "" {
			from += " " + s.Window
		}
		stopped := "-"
		if !s.StoppedAt.IsZero() {
			stopped = s.StoppedAt.Local().Format("Mon 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n", s.Status, label(s), stopped,
			next, from, s.Attempts, state.MaxAttempts, s.Note)
	}
	tw.Flush()
}

func label(s state.Stop) string {
	name := s.Title
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:39]) + "…"
	}
	if name == "" {
		return s.SessionID
	}
	return name + " " + s.SessionID
}

func until(t, now time.Time) string {
	d := t.Sub(now).Round(time.Minute)
	if d <= 0 {
		return "due"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h >= 24 {
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
