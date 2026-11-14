// Package state holds the tracked stops, their transitions (AGENTS.md §3 "Resuming a due stop") and
// the state file, written atomically.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Greqory/letsgoagain/internal/reset"
)

// Stop statuses. Waiting and Resumed are open; the others are closed.
const (
	Waiting = "waiting"  // planned, not yet resumed
	Resumed = "resumed"  // resume sent, waiting for the execution to succeed
	Done    = "done"     // the session ran successfully after the stop
	MovedOn = "moved_on" // the session changed without us (someone else continued it)
	Failed  = "failed"   // MaxAttempts resumes did not get it past the limit
	Gone    = "gone"     // the session no longer exists
	DryRun  = "dry_run"  // due, but the daemon only watches
)

// MaxAttempts is how many resumes one stop gets.
const MaxAttempts = 5

// keepClosed is how long closed stops stay in the file for `status`.
const keepClosed = 7 * 24 * time.Hour

// Stop is one session stopped by a usage limit.
type Stop struct {
	SessionID  string    `json:"sessionID"`
	Title      string    `json:"title,omitempty"`
	Directory  string    `json:"directory,omitempty"`
	StoppedAt  time.Time `json:"stoppedAt"`
	Idle       int64     `json:"idle"` // the session's time.idle at the stop: unchanged = nobody touched it
	RetryAfter int       `json:"retryAfter,omitempty"`
	NextAt     time.Time `json:"nextAt"`
	Source     string    `json:"source"`
	Window     string    `json:"window,omitempty"`
	Attempts   int       `json:"attempts"`
	Status     string    `json:"status"`
	Note       string    `json:"note,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// NoticeID is the inbox item showing the stop in the TUI; NoticeFor the time it announces.
	NoticeID  string    `json:"noticeID,omitempty"`
	NoticeFor time.Time `json:"noticeFor,omitempty"`
}

// Open reports whether the stop still needs LetsGoAgain.
func (s *Stop) Open() bool { return s.Status == Waiting || s.Status == Resumed }

// State is everything in the state file.
type State struct {
	Stops map[string]*Stop `json:"stops"`
}

// New returns an empty state.
func New() *State { return &State{Stops: map[string]*Stop{}} }

// Load reads the state file; a missing file is an empty state.
func Load(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	s := New()
	if err := json.Unmarshal(raw, s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.Stops == nil {
		s.Stops = map[string]*Stop{}
	}
	return s, nil
}

// Save writes the state file atomically, dropping closed stops older than a week.
func (s *State) Save(path string, now time.Time) error {
	for id, st := range s.Stops {
		if !st.Open() && now.Sub(st.UpdatedAt) > keepClosed {
			delete(s.Stops, id)
		}
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// Sorted returns the stops, open ones first, each group by next time.
func (s *State) Sorted() []*Stop {
	out := make([]*Stop, 0, len(s.Stops))
	for _, st := range s.Stops {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Open() != out[j].Open() {
			return out[i].Open()
		}
		return out[i].NextAt.Before(out[j].NextAt)
	})
	return out
}

// Limit is a limit failure of a session as the daemon saw it.
type Limit struct {
	SessionID  string
	Title      string
	Directory  string
	StoppedAt  time.Time
	Idle       int64
	RetryAfter int
}

// RecordLimit applies a limit failure and returns the open stop, or false when it is not a new event
// (the same failure seen again, e.g. by a scan) or the stop has used up its attempts.
func (s *State) RecordLimit(l Limit, now time.Time, plan func() reset.Plan) (*Stop, bool) {
	st := s.Stops[l.SessionID]
	if st != nil && st.Open() && st.Idle == l.Idle {
		return st, false
	}
	if st == nil || !st.Open() {
		st = &Stop{SessionID: l.SessionID}
		s.Stops[l.SessionID] = st
	}
	st.Title, st.Directory = l.Title, l.Directory
	st.StoppedAt, st.Idle, st.RetryAfter = l.StoppedAt, l.Idle, l.RetryAfter
	st.UpdatedAt = now
	if st.Attempts >= MaxAttempts {
		st.Status, st.Note = Failed, fmt.Sprintf("still limited after %d resumes", st.Attempts)
		return st, false
	}
	st.Status = Waiting
	st.SetPlan(plan())
	st.Note = ""
	return st, true
}

// SetPlan sets when the stop is looked at next.
func (st *Stop) SetPlan(p reset.Plan) {
	st.NextAt, st.Source, st.Window = p.At, p.Source, p.Window
}

// Plan returns the stop's current plan.
func (st *Stop) Plan() reset.Plan {
	return reset.Plan{At: st.NextAt, Source: st.Source, Window: st.Window}
}

// Started handles a session that began executing: unless we resumed it, someone else did.
func (s *State) Started(id string, now time.Time) (*Stop, bool) {
	st := s.Stops[id]
	if st == nil || st.Status != Waiting {
		return st, false
	}
	st.Status, st.Note, st.UpdatedAt = MovedOn, "continued without LetsGoAgain", now
	return st, true
}

// Succeeded closes an open stop of a session that ran successfully.
func (s *State) Succeeded(id string, now time.Time) (*Stop, bool) {
	st := s.Stops[id]
	if st == nil || !st.Open() {
		return st, false
	}
	st.Status, st.Note, st.UpdatedAt = Done, "", now
	return st, true
}

// Close ends a stop with a closed status and a note.
func (st *Stop) Close(status, note string, now time.Time) {
	st.Status, st.Note, st.UpdatedAt = status, note, now
}

// MarkResumed records a sent resume.
func (st *Stop) MarkResumed(now time.Time) {
	st.Attempts++
	st.Status, st.Note, st.UpdatedAt = Resumed, "", now
}
