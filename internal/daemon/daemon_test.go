package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/opencode"
	"github.com/Greqory/letsgoagain/internal/reset"
	"github.com/Greqory/letsgoagain/internal/state"
)

const limitErr = `{"type":"provider.rate-limit","message":"rate_limit_error: Claude Max rate limit reached. Wait a moment and try again.","status":200,"response":{"body":"{\"error\":{\"message\":\"Claude Max rate limit reached. Wait a moment and try again.\",\"retry_after\":4868,\"type\":\"rate_limit_error\"},\"type\":\"error\"}"}}`

type fakeAPI struct {
	mu        sync.Mutex
	sessions  map[string]opencode.Session
	messages  map[string][]opencode.Message // oldest first, as OpenCode writes them
	running   map[string]bool
	prompts   []string
	promptErr error
	version   string
	inbox     map[string]string // inbox ID → description
	noticeErr error
	calls     []string // "notice <desc>", "cancel <id>", "prompt <session>", in order
	nextID    int
}

func (f *fakeAPI) AddNotice(_ context.Context, id, description, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noticeErr != nil {
		return "", f.noticeErr
	}
	if !strings.Contains(text, "LetsGoAgain") {
		return "", fmt.Errorf("notice text %q does not name LetsGoAgain", text)
	}
	f.nextID++
	inboxID := fmt.Sprintf("msg_notice%d", f.nextID)
	f.inbox[inboxID] = description
	f.calls = append(f.calls, "notice "+description)
	return inboxID, nil
}

func (f *fakeAPI) CancelNotice(_ context.Context, _, inboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cancel "+inboxID)
	if _, ok := f.inbox[inboxID]; !ok {
		return fmt.Errorf("DELETE inbox: %w", opencode.ErrNotFound)
	}
	delete(f.inbox, inboxID)
	return nil
}

func (f *fakeAPI) inboxDescriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, d := range f.inbox {
		out = append(out, d)
	}
	return out
}

func (f *fakeAPI) Version() string { return f.version }

func newFakeAPI() *fakeAPI {
	return &fakeAPI{sessions: map[string]opencode.Session{}, messages: map[string][]opencode.Message{}, running: map[string]bool{},
		inbox: map[string]string{}}
}

// append adds messages to a session (creating it) and moves its outcome and times along.
func (f *fakeAPI) append(id, parent string, at time.Time, outcome string, msgs ...opencode.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		s = opencode.Session{ID: id, ParentID: parent, Title: "work " + id}
		s.Location.Directory = "/repo"
		f.messages[id] = []opencode.Message{f.msg("user", at, "")}
	}
	for _, m := range msgs {
		m.Time.Created = at.UnixMilli()
		f.messages[id] = append(f.messages[id], m)
	}
	s.Outcome, s.Time.Idle, s.Time.Updated = outcome, at.UnixMilli(), at.UnixMilli()
	f.sessions[id] = s
}

func (f *fakeAPI) msg(typ string, at time.Time, errJSON string) opencode.Message {
	f.nextID++
	m := opencode.Message{ID: fmt.Sprintf("msg_%s%d", typ, f.nextID), Type: typ}
	m.Time.Created = at.UnixMilli()
	if errJSON != "" {
		m.Error, m.Outcome = json.RawMessage(errJSON), "failed"
	}
	return m
}

// addFailedWith: the session's newest run failed with errJSON, as OpenCode records it.
func (f *fakeAPI) addFailedWith(id, parent string, at time.Time, errJSON string) {
	f.mu.Lock()
	assistant, idle := f.msg("assistant", at, errJSON), f.msg("idle", at, errJSON)
	f.mu.Unlock()
	f.append(id, parent, at, "failed", assistant, idle)
}

func (f *fakeAPI) addFailed(id, parent string, at time.Time) {
	f.addFailedWith(id, parent, at, limitErr)
}

// emptyRun is what OpenCode sometimes does right after a limit failure: an execution with no model
// call and no message, ending "succeeded" (seen 2026-11-14).
func (f *fakeAPI) emptyRun(id string, at time.Time) {
	f.mu.Lock()
	idle := f.msg("idle", at, "")
	f.mu.Unlock()
	idle.Outcome = "succeeded"
	f.append(id, "", at, "succeeded", idle)
}

// goesOn: someone sent a message and the agent answered.
func (f *fakeAPI) goesOn(id string, at time.Time) {
	f.mu.Lock()
	user, reply, idle := f.msg("user", at, ""), f.msg("assistant", at, ""), f.msg("idle", at, "")
	f.mu.Unlock()
	idle.Outcome = "succeeded"
	f.append(id, "", at, "succeeded", user, reply, idle)
}

// agentRuns: after a resume, the agent works and the run succeeds.
func (f *fakeAPI) agentRuns(id string, at time.Time) {
	f.mu.Lock()
	reply, idle := f.msg("assistant", at, ""), f.msg("idle", at, "")
	f.mu.Unlock()
	idle.Outcome = "succeeded"
	f.append(id, "", at, "succeeded", reply, idle)
}

func (f *fakeAPI) Session(_ context.Context, id string) (opencode.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return s, fmt.Errorf("GET session: %w", opencode.ErrNotFound)
	}
	return s, nil
}

func (f *fakeAPI) RootSessions(context.Context, int) ([]opencode.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []opencode.Session
	for _, s := range f.sessions {
		if s.ParentID == "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeAPI) RecentMessages(_ context.Context, id string, n int) ([]opencode.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.messages[id]
	var out []opencode.Message
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

func (f *fakeAPI) Running(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id], nil
}

func (f *fakeAPI) Prompt(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promptErr != nil {
		return f.promptErr
	}
	f.prompts = append(f.prompts, id+": "+text)
	f.calls = append(f.calls, "prompt "+id)
	user := f.msg("user", time.UnixMilli(f.sessions[id].Time.Updated), "")
	f.messages[id] = append(f.messages[id], user)
	return nil
}

func (f *fakeAPI) Events(ctx context.Context, _ []string, connected func(), _ func(opencode.Event) error) error {
	connected()
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeAPI) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

type fakeQuota struct {
	q       meridian.Quota
	err     error
	version string
}

func (f *fakeQuota) Version(context.Context) (string, error) {
	if f.version == "" {
		return "", errors.New("no /health")
	}
	return f.version, nil
}

func (f *fakeQuota) Quota(context.Context) (meridian.Quota, error) { return f.q, f.err }

func (f *fakeQuota) spent(window string, at time.Time) {
	ms := at.UnixMilli()
	f.q, f.err = meridian.Quota{Buckets: []meridian.Bucket{{Type: window, Status: "rejected", ResetsAt: &ms}}}, nil
}

func (f *fakeQuota) clear() { f.q, f.err = meridian.Quota{}, nil }

type harness struct {
	t     *testing.T
	d     *Daemon
	api   *fakeAPI
	quota *fakeQuota
	now   time.Time
	log   *bytes.Buffer
}

var stopTime = time.Date(2026, 11, 13, 1, 37, 0, 0, time.UTC)

func newHarness(t *testing.T, dryRun bool) *harness {
	return newHarnessWith(t, func(c *Config) { c.DryRun = dryRun })
}

func newHarnessWith(t *testing.T, configure func(*Config)) *harness {
	h := &harness{t: t, api: newFakeAPI(), quota: &fakeQuota{}, now: stopTime.Add(5 * time.Second), log: &bytes.Buffer{}}
	cfg := Config{
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		Connect:   func() (API, error) { return h.api, nil },
		Quota:     h.quota,
		Log:       log.New(h.log, "", 0),
		Now:       func() time.Time { return h.now },
	}
	configure(&cfg)
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.d = d
	d.handle(context.Background(), connected{h.api})
	return h
}

func (h *harness) event(typ, id string) {
	ev := opencode.Event{Type: typ}
	ev.Data.SessionID = id
	if typ == opencode.EventExecutionFailed {
		ev.Data.Error = json.RawMessage(limitErr)
	}
	h.d.handle(context.Background(), eventMsg{ev})
}

func (h *harness) at(t time.Time) {
	h.now = t
	h.d.handle(context.Background(), tick{})
}

func (h *harness) stop(id string) *state.Stop {
	st := h.d.st.Stops[id]
	if st == nil {
		h.t.Fatalf("no stop for %s; log:\n%s", id, h.log)
	}
	return st
}

func TestResumeAtQuotaReset(t *testing.T) {
	h := newHarness(t, false)
	resetAt := stopTime.Add(81 * time.Minute)
	h.quota.spent("five_hour", resetAt)
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")

	st := h.stop("ses_a")
	if st.Status != state.Waiting || st.Source != reset.SourceQuota || st.Window != "five_hour" || !st.NextAt.Equal(resetAt.Add(reset.Margin)) {
		t.Fatalf("recorded %+v", st)
	}
	h.at(resetAt) // within the margin
	if h.api.promptCount() != 0 {
		t.Fatal("resumed before the reset")
	}
	h.quota.clear()
	h.at(resetAt.Add(reset.Margin))
	if h.api.promptCount() != 1 || h.api.prompts[0] != "ses_a: "+DefaultMessage || st.Status != state.Resumed || st.Attempts != 1 {
		t.Fatalf("prompts %v, stop %+v", h.api.prompts, st)
	}
	h.event(opencode.EventExecutionStarted, "ses_a")
	if st.Status != state.Resumed {
		t.Fatalf("our own resume closed the stop: %+v", st)
	}
	h.api.agentRuns("ses_a", h.now)
	h.event(opencode.EventExecutionSucceeded, "ses_a")
	if st.Status != state.Done {
		t.Fatalf("after success %+v", st)
	}
	saved, err := state.Load(h.d.cfg.StatePath)
	if err != nil || saved.Stops["ses_a"].Status != state.Done {
		t.Fatalf("state file: %+v %v", saved, err)
	}
	for _, want := range []string{"limit stop recorded, reset from the meridian quota (five_hour)", "resume sent (reset reached, attempt 1/5)", "stop done"} {
		if !strings.Contains(h.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, h.log)
		}
	}
}

// What a session receives is the user's call (AGENTS.md §5): a change here must be deliberate.
func TestDefaultMessage(t *testing.T) {
	if DefaultMessage != "The Claude usage limit has reset. Continue where you left off." {
		t.Fatalf("the resume message changed: %q", DefaultMessage)
	}
}

func TestRetryAfterWhenQuotaUnavailable(t *testing.T) {
	h := newHarness(t, false)
	h.quota.err = errors.New("connection refused")
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	st := h.stop("ses_a")
	want := stopTime.Add(4868*time.Second + reset.Margin)
	if st.Source != reset.SourceRetryAfter || !st.NextAt.Equal(want) {
		t.Fatalf("recorded %+v", st)
	}
	h.at(want)
	if h.api.promptCount() != 1 {
		t.Fatalf("not resumed; log:\n%s", h.log)
	}
}

func TestStillSpentAtDueReschedules(t *testing.T) {
	h := newHarness(t, false)
	h.quota.err = errors.New("down")
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	weekly := stopTime.Add(49 * time.Hour)
	h.quota.spent("seven_day", weekly)
	h.at(stopTime.Add(4868*time.Second + reset.Margin))
	st := h.stop("ses_a")
	if h.api.promptCount() != 0 || st.Attempts != 0 || st.Source != reset.SourceQuota || !st.NextAt.Equal(weekly.Add(reset.Margin)) {
		t.Fatalf("prompts %d, stop %+v", h.api.promptCount(), st)
	}
}

func TestIgnoresChildAndOtherFailures(t *testing.T) {
	h := newHarness(t, false)
	h.api.addFailed("ses_child", "ses_root", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_child")
	ev := opencode.Event{Type: opencode.EventExecutionFailed}
	ev.Data.SessionID = "ses_other"
	ev.Data.Error = json.RawMessage(`{"type":"aborted","message":"Step interrupted"}`)
	h.d.handle(context.Background(), eventMsg{ev})
	if len(h.d.st.Stops) != 0 {
		t.Fatalf("stops %+v", h.d.st.Stops)
	}
}

func TestMovedOn(t *testing.T) {
	t.Run("user continued", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.event(opencode.EventExecutionFailed, "ses_a")
		h.event(opencode.EventExecutionStarted, "ses_a")
		h.api.goesOn("ses_a", stopTime.Add(time.Minute))
		h.event(opencode.EventExecutionSucceeded, "ses_a")
		if st := h.stop("ses_a"); st.Status != state.MovedOn {
			t.Fatalf("stop %+v", st)
		}
		h.at(stopTime.Add(6 * time.Hour))
		if h.api.promptCount() != 0 {
			t.Fatalf("prompts %d", h.api.promptCount())
		}
	})
	t.Run("changed while the daemon missed it", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.event(opencode.EventExecutionFailed, "ses_a")
		h.api.goesOn("ses_a", stopTime.Add(time.Minute))
		h.at(stopTime.Add(6 * time.Hour))
		if st := h.stop("ses_a"); st.Status != state.MovedOn || h.api.promptCount() != 0 {
			t.Fatalf("stop %+v, prompts %d", st, h.api.promptCount())
		}
	})
	t.Run("stopped at the limit again while the daemon missed it", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.event(opencode.EventExecutionFailed, "ses_a")
		first := h.stop("ses_a").MessageID
		h.api.goesOn("ses_a", stopTime.Add(time.Minute))
		h.api.addFailed("ses_a", "", stopTime.Add(2*time.Minute))
		h.at(stopTime.Add(6 * time.Hour))
		st := h.stop("ses_a")
		if st.Status != state.Waiting || st.MessageID == first || !st.StoppedAt.Equal(stopTime.Add(2*time.Minute)) || h.api.promptCount() != 0 {
			t.Fatalf("stop %+v, prompts %d", st, h.api.promptCount())
		}
	})
	t.Run("running", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.event(opencode.EventExecutionFailed, "ses_a")
		h.api.running["ses_a"] = true
		h.at(stopTime.Add(6 * time.Hour))
		if st := h.stop("ses_a"); st.Status != state.MovedOn || h.api.promptCount() != 0 {
			t.Fatalf("stop %+v, prompts %d", st, h.api.promptCount())
		}
	})
}

// The 2026-11-14 miss: OpenCode ran an empty execution 5 ms after the limit failure, and the stop was
// closed as "continued without LetsGoAgain".
func TestEmptyRunAfterTheStop(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.event(opencode.EventExecutionFailed, "ses_a")
		h.event(opencode.EventExecutionStarted, "ses_a")
		h.api.emptyRun("ses_a", stopTime.Add(5*time.Millisecond))
		h.event(opencode.EventExecutionSucceeded, "ses_a")
		st := h.stop("ses_a")
		if st.Status != state.Waiting || len(h.api.inboxDescriptions()) != 1 || !strings.Contains(h.log.String(), "empty execution after the stop; still waiting") {
			t.Fatalf("stop %+v, inbox %v; log:\n%s", st, h.api.inboxDescriptions(), h.log)
		}
		h.at(st.NextAt)
		if h.api.promptCount() != 1 || st.Status != state.Resumed {
			t.Fatalf("not resumed: prompts %d, stop %+v; log:\n%s", h.api.promptCount(), st, h.log)
		}
	})
	t.Run("found by the scan", func(t *testing.T) {
		h := newHarness(t, false)
		h.api.addFailed("ses_a", "", stopTime)
		h.api.emptyRun("ses_a", stopTime.Add(5*time.Millisecond))
		h.d.handle(context.Background(), connected{h.api})
		st := h.stop("ses_a")
		if st.Status != state.Waiting || !st.StoppedAt.Equal(stopTime) {
			t.Fatalf("stop %+v", st)
		}
		h.d.handle(context.Background(), connected{h.api}) // a later scan leaves it alone
		if st.Status != state.Waiting || strings.Count(h.log.String(), "limit stop recorded") != 1 {
			t.Fatalf("stop %+v; log:\n%s", st, h.log)
		}
	})
}

func TestDryRunSendsNothing(t *testing.T) {
	h := newHarness(t, true)
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	h.at(stopTime.Add(6 * time.Hour))
	if st := h.stop("ses_a"); st.Status != state.DryRun || h.api.promptCount() != 0 {
		t.Fatalf("stop %+v, prompts %d", st, h.api.promptCount())
	}
	if !strings.Contains(h.log.String(), "dry run, would resume now") {
		t.Fatalf("log:\n%s", h.log)
	}
}

func TestGivesUpAfterFiveResumes(t *testing.T) {
	h := newHarness(t, false)
	h.quota.err = errors.New("down")
	idle := stopTime
	h.api.addFailed("ses_a", "", idle)
	h.event(opencode.EventExecutionFailed, "ses_a")
	for i := 1; i <= state.MaxAttempts; i++ {
		h.at(h.stop("ses_a").NextAt)
		if h.api.promptCount() != i {
			t.Fatalf("attempt %d: prompts %d; log:\n%s", i, h.api.promptCount(), h.log)
		}
		idle = h.now.Add(80 * time.Second)
		h.api.addFailed("ses_a", "", idle) // the resume ran into the limit again
		h.now = idle
		h.event(opencode.EventExecutionFailed, "ses_a")
	}
	if st := h.stop("ses_a"); st.Status != state.Failed {
		t.Fatalf("stop %+v", st)
	}
	h.at(h.now.Add(10 * time.Hour))
	if h.api.promptCount() != state.MaxAttempts {
		t.Fatalf("resumed after giving up: %d", h.api.promptCount())
	}
}

func TestUnreachableOrGone(t *testing.T) {
	h := newHarness(t, false)
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	due := h.stop("ses_a").NextAt

	h.d.handle(context.Background(), disconnected{})
	h.at(due)
	if st := h.stop("ses_a"); st.Status != state.Waiting {
		t.Fatalf("disconnected: %+v", st)
	}
	h.d.handle(context.Background(), connected{h.api})
	h.api.promptErr = errors.New("status 500")
	h.at(due.Add(time.Minute))
	if st := h.stop("ses_a"); st.Status != state.Waiting || h.api.promptCount() != 0 || !st.NextAt.After(h.now) {
		t.Fatalf("prompt error: %+v", st)
	}
	delete(h.api.sessions, "ses_a")
	h.at(h.now.Add(time.Hour))
	if st := h.stop("ses_a"); st.Status != state.Gone {
		t.Fatalf("gone: %+v", st)
	}
}

func TestScanOnConnect(t *testing.T) {
	h := newHarness(t, false)
	h.now = stopTime.Add(time.Hour)
	h.api.addFailed("ses_recent", "", stopTime)
	h.api.addFailed("ses_old", "", stopTime.Add(-13*time.Hour))
	h.api.addFailedWith("ses_other", "", stopTime, `{"type":"aborted","message":"Step interrupted"}`)
	h.d.handle(context.Background(), connected{h.api})
	if len(h.d.st.Stops) != 1 || h.d.st.Stops["ses_recent"] == nil {
		t.Fatalf("stops %+v", h.d.st.Stops)
	}
	h.d.handle(context.Background(), connected{h.api}) // a reconnect sees the same failure
	if strings.Count(h.log.String(), "limit stop recorded") != 1 {
		t.Fatalf("recorded twice:\n%s", h.log)
	}
	h.api.goesOn("ses_recent", stopTime.Add(30*time.Minute))
	h.d.handle(context.Background(), connected{h.api})
	if st := h.stop("ses_recent"); st.Status != state.MovedOn {
		t.Fatalf("after someone went on, seen by the scan: %+v", st)
	}
}

func notice(at time.Time) string {
	return "LetsGoAgain · Claude limit reached · continues automatically " + at.Local().Format("Mon 15:04")
}

func TestNoticeLifecycle(t *testing.T) {
	h := newHarness(t, false)
	h.quota.err = errors.New("down")
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	st := h.stop("ses_a")
	first := st.NextAt
	if got := h.api.inboxDescriptions(); len(got) != 1 || got[0] != notice(first) || st.NoticeID == "" {
		t.Fatalf("notice at the stop: %v, stop %+v", got, st)
	}

	// Due, but the quota now names a later reset: the notice moves with it.
	weekly := stopTime.Add(49 * time.Hour)
	h.quota.spent("seven_day", weekly)
	h.at(first)
	if got := h.api.inboxDescriptions(); len(got) != 1 || got[0] != notice(weekly.Add(reset.Margin)) {
		t.Fatalf("notice after the reschedule: %v", got)
	}

	// The resume: the notice is gone before the prompt goes out.
	h.quota.clear()
	h.at(weekly.Add(reset.Margin))
	n := len(h.api.calls)
	if n < 2 || !strings.HasPrefix(h.api.calls[n-2], "cancel ") || h.api.calls[n-1] != "prompt ses_a" || len(h.api.inboxDescriptions()) != 0 {
		t.Fatalf("calls %v, inbox %v", h.api.calls, h.api.inboxDescriptions())
	}
	if st.NoticeID != "" {
		t.Fatalf("stop still points at a notice: %+v", st)
	}
}

func TestNoticeRemovedWhenTheStopEndsOtherwise(t *testing.T) {
	for name, end := range map[string]func(h *harness){
		"continued by hand": func(h *harness) {
			h.api.goesOn("ses_a", stopTime.Add(time.Minute))
			h.event(opencode.EventExecutionSucceeded, "ses_a")
		},
		"changed unseen": func(h *harness) {
			h.api.goesOn("ses_a", stopTime.Add(time.Minute))
			h.at(stopTime.Add(6 * time.Hour))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, false)
			h.api.addFailed("ses_a", "", stopTime)
			h.event(opencode.EventExecutionFailed, "ses_a")
			end(h)
			if st := h.stop("ses_a"); st.Status != state.MovedOn || len(h.api.inboxDescriptions()) != 0 || h.api.promptCount() != 0 {
				t.Fatalf("stop %+v, inbox %v", st, h.api.inboxDescriptions())
			}
		})
	}
}

func TestNoNoticeWhenDisabledOrDryRun(t *testing.T) {
	for name, configure := range map[string]func(*Config){
		"--no-notice": func(c *Config) { c.NoNotice = true },
		"--dry-run":   func(c *Config) { c.DryRun = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessWith(t, configure)
			h.api.addFailed("ses_a", "", stopTime)
			h.event(opencode.EventExecutionFailed, "ses_a")
			if len(h.api.calls) != 0 {
				t.Fatalf("calls %v", h.api.calls)
			}
		})
	}
}

func TestNoticeFailureDoesNotStopTheResume(t *testing.T) {
	h := newHarness(t, false)
	h.api.noticeErr = errors.New("status 500")
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")
	h.at(h.stop("ses_a").NextAt)
	if h.api.promptCount() != 1 || !strings.Contains(h.log.String(), "TUI notice not shown") {
		t.Fatalf("prompts %d; log:\n%s", h.api.promptCount(), h.log)
	}
}

func TestProbeNotice(t *testing.T) {
	h := newHarness(t, false)
	noRetryAfter := `{"type":"provider.rate-limit","message":"m","response":{"body":"{}"}}`
	h.api.addFailedWith("ses_a", "", stopTime, noRetryAfter)
	ev := opencode.Event{Type: opencode.EventExecutionFailed}
	ev.Data.SessionID = "ses_a"
	ev.Data.Error = json.RawMessage(noRetryAfter)
	h.d.handle(context.Background(), eventMsg{ev})
	want := "LetsGoAgain · Claude limit reached · reset unknown, checking again " + h.stop("ses_a").NextAt.Local().Format("Mon 15:04")
	if got := h.api.inboxDescriptions(); len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want %q", got, want)
	}
}

func TestVersionNotes(t *testing.T) {
	h := newHarness(t, false) // connected once with unknown versions
	if !strings.Contains(h.log.String(), "warning: OpenCode version unknown") || !strings.Contains(h.log.String(), "meridian version unavailable") {
		t.Fatalf("log:\n%s", h.log)
	}
	h.log.Reset()
	h.api.version, h.quota.version = "2.3.0", "1.79.0"
	h.d.handle(context.Background(), connected{h.api})
	if got := h.log.String(); !strings.Contains(got, "OpenCode 2.3.0 (tested: 2.3)") || !strings.Contains(got, "meridian 1.79.0 (tested: 1.79)") || strings.Contains(got, "warning") {
		t.Fatalf("tested versions:\n%s", got)
	}
	h.log.Reset()
	h.d.handle(context.Background(), connected{h.api})
	if strings.Contains(h.log.String(), "OpenCode 2.3.0") {
		t.Fatalf("a reconnect with the same versions logged them again:\n%s", h.log)
	}
	h.api.version = "2.4.1"
	h.d.handle(context.Background(), connected{h.api})
	if !strings.Contains(h.log.String(), "warning: OpenCode 2.4.1 is newer than the tested 2.3") {
		t.Fatalf("newer version:\n%s", h.log)
	}
}

func TestControlOverSocket(t *testing.T) {
	h := newHarness(t, false)
	h.api.addFailed("ses_a", "", stopTime)
	h.event(opencode.EventExecutionFailed, "ses_a")

	dir, err := os.MkdirTemp("", "lga") // a short path: Unix socket paths are limited to ~108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	runDone, serveDone := make(chan struct{}), make(chan struct{})
	go func() { h.d.Run(ctx); close(runDone) }()
	go func() { h.d.Serve(ctx, sock); close(serveDone) }()
	// Cleanups run last-in first-out: this one ends both before the state's TempDir is removed, which
	// otherwise races a final state save (seen once in 200 runs).
	t.Cleanup(func() { cancel(); <-runDone; <-serveDone })
	c := NewControl(sock)
	var stops []state.Stop
	for i := 0; i < 100; i++ {
		if stops, err = c.Status(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(stops) != 1 || stops[0].Status != state.Waiting {
		t.Fatalf("status: %+v %v", stops, err)
	}
	if _, err := c.ResumeNow("ses_nope"); err == nil || !strings.Contains(err.Error(), "no waiting stop for ses_nope") {
		t.Fatalf("unknown id: %v", err)
	}
	res, err := c.ResumeNow("")
	if err != nil || len(res) != 1 || !strings.HasSuffix(res[0], ": resumed") || h.api.promptCount() != 1 {
		t.Fatalf("resume-now: %v %v, prompts %d", res, err, h.api.promptCount())
	}
	if _, err := c.ResumeNow(""); err == nil {
		t.Fatal("nothing left to resume, want an error")
	}
}
