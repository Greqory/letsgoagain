package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Greqory/letsgoagain/internal/reset"
)

var t0 = time.Date(2026, 11, 13, 1, 37, 0, 0, time.UTC)

func plan(at time.Time) func() reset.Plan {
	return func() reset.Plan { return reset.Plan{At: at, Source: reset.SourceRetryAfter} }
}

func limit(idle int64) Limit {
	return Limit{SessionID: "ses_a", Title: "t", StoppedAt: time.UnixMilli(idle), Idle: idle, RetryAfter: 4868}
}

func TestLifecycle(t *testing.T) {
	s := New()
	st, isNew := s.RecordLimit(limit(1000), t0, plan(t0.Add(time.Hour)))
	if !isNew || st.Status != Waiting || st.Attempts != 0 || !st.NextAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("first record: %+v %v", st, isNew)
	}
	if _, isNew := s.RecordLimit(limit(1000), t0, plan(t0)); isNew {
		t.Fatal("the same failure seen again must not be new")
	}
	st.MarkResumed(t0.Add(time.Hour))
	if _, changed := s.Started("ses_a", t0); changed || st.Status != Resumed {
		t.Fatal("our own resume starting the session must not close the stop")
	}
	// The resume hit the limit again: same stop, attempts kept.
	st2, isNew := s.RecordLimit(limit(2000), t0, plan(t0.Add(2*time.Hour)))
	if !isNew || st2 != st || st.Attempts != 1 || st.Status != Waiting {
		t.Fatalf("repeat: %+v %v", st, isNew)
	}
	st.MarkResumed(t0)
	if _, ok := s.Succeeded("ses_a", t0); !ok || st.Status != Done {
		t.Fatalf("succeeded: %+v", st)
	}
	// A later limit after a success is a new stop.
	st3, isNew := s.RecordLimit(limit(3000), t0, plan(t0))
	if !isNew || st3.Attempts != 0 || st3.Status != Waiting {
		t.Fatalf("new episode: %+v", st3)
	}
}

func TestMovedOn(t *testing.T) {
	s := New()
	s.RecordLimit(limit(1000), t0, plan(t0))
	if st, ok := s.Started("ses_a", t0); !ok || st.Status != MovedOn {
		t.Fatalf("got %+v", st)
	}
	if _, ok := s.Succeeded("ses_a", t0); ok {
		t.Fatal("a closed stop does not change on success")
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	s := New()
	st, _ := s.RecordLimit(limit(0), t0, plan(t0))
	for i := 1; i <= MaxAttempts; i++ {
		st.MarkResumed(t0)
		_, isNew := s.RecordLimit(limit(int64(i)), t0, plan(t0))
		if want := i < MaxAttempts; isNew != want {
			t.Fatalf("attempt %d: isNew=%v", i, isNew)
		}
	}
	if st.Status != Failed || st.Open() {
		t.Fatalf("got %+v", st)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Load(path)
	if err != nil || len(s.Stops) != 0 {
		t.Fatalf("missing file: %+v %v", s, err)
	}
	s.RecordLimit(limit(1000), t0, plan(t0.Add(time.Hour)))
	old := &Stop{SessionID: "ses_old", Status: Done, UpdatedAt: t0.Add(-8 * 24 * time.Hour)}
	recent := &Stop{SessionID: "ses_recent", Status: Done, UpdatedAt: t0.Add(-time.Hour)}
	s.Stops[old.SessionID], s.Stops[recent.SessionID] = old, recent
	if err := s.Save(path, t0); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Stops) != 2 || got.Stops["ses_old"] != nil || !got.Stops["ses_a"].NextAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("got %+v", got.Stops)
	}
	if sorted := got.Sorted(); sorted[0].SessionID != "ses_a" {
		t.Fatalf("open stops first, got %s", sorted[0].SessionID)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want an error for a broken file")
	}
}
