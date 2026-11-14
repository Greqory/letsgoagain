// Package daemon is LetsGoAgain's watcher: it follows the OpenCode service's events, records limit
// stops, checks the wall clock and resumes stops whose reset has come (AGENTS.md §3). One goroutine owns
// the state; events, ticks and control requests reach it through a channel.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Greqory/letsgoagain/internal/compat"
	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/opencode"
	"github.com/Greqory/letsgoagain/internal/reset"
	"github.com/Greqory/letsgoagain/internal/state"
)

// DefaultMessage is what a resumed session receives. Naming the limit tells the agent why its work broke
// off, so it checks where it stood instead of assuming its last step completed.
const DefaultMessage = "The Claude usage limit has reset. Continue where you left off."

// API is the part of the OpenCode service the daemon uses.
type API interface {
	Session(ctx context.Context, id string) (opencode.Session, error)
	RootSessions(ctx context.Context, limit int) ([]opencode.Session, error)
	RecentMessages(ctx context.Context, id string, n int) ([]opencode.Message, error)
	Running(ctx context.Context, id string) (bool, error)
	Prompt(ctx context.Context, id, text string) error
	AddNotice(ctx context.Context, id, description, text string) (string, error)
	CancelNotice(ctx context.Context, id, inboxID string) error
	Events(ctx context.Context, types []string, connected func(), handle func(opencode.Event) error) error
	Version() string
}

// Quota reads meridian's quota.
type Quota interface {
	Quota(ctx context.Context) (meridian.Quota, error)
}

// versioned is a Quota that can also tell meridian's version.
type versioned interface {
	Version(ctx context.Context) (string, error)
}

// Config wires a daemon.
type Config struct {
	StatePath  string
	Version    string
	Connect    func() (API, error) // reads service.json afresh each time
	Quota      Quota
	Log        *log.Logger
	DryRun     bool
	NoNotice   bool // no "continues automatically at …" line in the TUI
	Message    string
	Now        func() time.Time
	Tick       time.Duration // wall-clock check
	Reconnect  time.Duration // wait after the service was unreachable
	ScanWindow time.Duration // how old a stop found by a scan may be
}

func (c *Config) defaults() {
	if c.Message == "" {
		c.Message = DefaultMessage
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Tick == 0 {
		c.Tick = 30 * time.Second
	}
	if c.Reconnect == 0 {
		c.Reconnect = 30 * time.Second
	}
	if c.ScanWindow == 0 {
		c.ScanWindow = 12 * time.Hour
	}
}

// Daemon watches and resumes.
type Daemon struct {
	cfg      Config
	st       *state.State
	api      API // nil while the service is unreachable
	msgs     chan any
	versions string // the last logged version notes, so a reconnect logs only changes
}

type connected struct{ api API }
type disconnected struct{}
type eventMsg struct{ ev opencode.Event }
type tick struct{}

// New loads the state and builds a daemon.
func New(cfg Config) (*Daemon, error) {
	cfg.defaults()
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	return &Daemon{cfg: cfg, st: st, msgs: make(chan any, 64)}, nil
}

// Run watches until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	mode := "resuming"
	if d.cfg.DryRun {
		mode = "dry run: recording only"
	}
	d.cfg.Log.Printf("started %s (%s), %d open stops", d.cfg.Version, mode, d.openCount())
	go d.connectLoop(ctx)
	go d.tickLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			d.cfg.Log.Printf("stopped")
			return nil
		case m := <-d.msgs:
			d.handle(ctx, m)
		}
	}
}

func (d *Daemon) send(ctx context.Context, m any) bool {
	select {
	case d.msgs <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Daemon) tickLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.send(ctx, tick{})
		}
	}
}

// A started execution says nothing: OpenCode starts empty ones after a limit failure, and a real
// continuation shows in the messages by the time it succeeds or fails.
var eventTypes = []string{opencode.EventExecutionFailed, opencode.EventExecutionSucceeded}

func (d *Daemon) connectLoop(ctx context.Context) {
	lastErr := ""
	for ctx.Err() == nil {
		api, err := d.cfg.Connect()
		if err == nil {
			err = api.Events(ctx, eventTypes,
				func() { lastErr = ""; d.send(ctx, connected{api}) },
				func(ev opencode.Event) error {
					if !d.send(ctx, eventMsg{ev}) {
						return ctx.Err()
					}
					return nil
				})
			d.send(ctx, disconnected{})
		}
		if ctx.Err() != nil {
			return
		}
		if msg := err.Error(); msg != lastErr {
			d.cfg.Log.Printf("OpenCode service unreachable, retrying every %s: %v", d.cfg.Reconnect, err)
			lastErr = msg
		}
		select {
		case <-ctx.Done():
		case <-time.After(d.cfg.Reconnect):
		}
	}
}

func (d *Daemon) handle(ctx context.Context, m any) {
	switch m := m.(type) {
	case connected:
		d.api = m.api
		d.cfg.Log.Printf("connected to the OpenCode service")
		d.logVersions(ctx)
		d.scan(ctx)
	case disconnected:
		d.api = nil
	case eventMsg:
		d.event(ctx, m.ev)
	case tick:
		d.due(ctx)
	case request:
		m.reply <- d.control(ctx, m)
	}
	d.save()
}

func (d *Daemon) event(ctx context.Context, ev opencode.Event) {
	id := ev.Data.SessionID
	if d.api == nil {
		return
	}
	switch ev.Type {
	case opencode.EventExecutionSucceeded:
		// Not proof that the session went on: OpenCode sometimes runs an empty execution right after a
		// limit failure. Only its messages tell.
		if st := d.st.Stops[id]; st != nil && st.Open() {
			d.checkWentOn(ctx, st)
		}
	case opencode.EventExecutionFailed:
		l, ok := opencode.ParseLimit(ev.Data.Error)
		if !ok {
			return
		}
		sess, err := d.api.Session(ctx, id)
		if err != nil {
			d.cfg.Log.Printf("%s: limit stop seen but the session could not be read: %v", id, err)
			return
		}
		if sess.ParentID != "" {
			return // a subagent: its root fails too and is the one resumed
		}
		stop, ok := d.stopOf(ctx, id)
		if !ok {
			// The messages don't show it (yet): take the event's word; the check before a resume then
			// accepts any limit stop it finds.
			d.cfg.Log.Printf("%s: limit failure not (yet) in the session's messages; going by the event", id)
			stop = opencode.LimitStop{At: d.cfg.Now().UnixMilli(), Limit: l}
		}
		d.record(ctx, sess, stop)
	}
}

// stopOf reads where a session stands; false also when its messages cannot be read.
func (d *Daemon) stopOf(ctx context.Context, id string) (opencode.LimitStop, bool) {
	msgs, err := d.api.RecentMessages(ctx, id, 10)
	if err != nil {
		d.cfg.Log.Printf("%s: messages not readable: %v", id, err)
		return opencode.LimitStop{}, false
	}
	return opencode.StopOf(msgs)
}

// sameStop reports whether the session still stands at the stop st recorded.
func sameStop(st *state.Stop, stop opencode.LimitStop) bool {
	return st.MessageID == "" || stop.MessageID == st.MessageID
}

// checkWentOn closes an open stop whose session has really moved past it.
func (d *Daemon) checkWentOn(ctx context.Context, st *state.Stop) {
	msgs, err := d.api.RecentMessages(ctx, st.SessionID, 10)
	if err != nil {
		d.cfg.Log.Printf("%s: messages not readable: %v", d.name(st), err)
		return
	}
	if stop, ok := opencode.StopOf(msgs); ok {
		if sameStop(st, stop) && st.Status == state.Waiting {
			d.cfg.Log.Printf("%s: OpenCode ran an empty execution after the stop; still waiting", d.name(st))
		}
		return // still at this stop, or at a new one that its failure event records
	}
	if st, ok := d.st.WentOn(st.SessionID, d.cfg.Now()); ok {
		d.clearNotice(ctx, st)
		if st.Status == state.Done {
			d.cfg.Log.Printf("%s: running again, stop done", d.name(st))
		} else {
			d.cfg.Log.Printf("%s: continued without LetsGoAgain, stop closed", d.name(st))
		}
	}
}

// logVersions notes the running OpenCode and meridian versions, with a warning when they differ from the
// tested ones; only when that changed since the last connect.
func (d *Daemon) logVersions(ctx context.Context) {
	notes := []string{}
	add := func(name, running, tested string) {
		note, ok := compat.Check(name, running, tested)
		if !ok {
			note = "warning: " + note
		}
		notes = append(notes, note)
	}
	add("OpenCode", d.api.Version(), compat.TestedOpenCode)
	if v, ok := d.cfg.Quota.(versioned); ok {
		running, err := v.Version(ctx)
		if err != nil {
			d.cfg.Log.Printf("meridian version unavailable: %v", err)
		} else {
			add("meridian", running, compat.TestedMeridian)
		}
	}
	joined := strings.Join(notes, "\n")
	if joined == d.versions {
		return
	}
	d.versions = joined
	for _, n := range notes {
		d.cfg.Log.Print(n)
	}
}

// scan finds limit stops that happened while the daemon was not listening.
func (d *Daemon) scan(ctx context.Context) {
	roots, err := d.api.RootSessions(ctx, 100)
	if err != nil {
		d.cfg.Log.Printf("scan: %v", err)
		return
	}
	since := d.cfg.Now().Add(-d.cfg.ScanWindow)
	for _, s := range roots {
		st := d.st.Stops[s.ID]
		open := st != nil && st.Open()
		if !open && time.UnixMilli(s.Time.Updated).Before(since) {
			continue
		}
		msgs, err := d.api.RecentMessages(ctx, s.ID, 10)
		if err != nil {
			d.cfg.Log.Printf("scan %s: %v", s.ID, err)
			continue
		}
		stop, atStop := opencode.StopOf(msgs)
		switch {
		case atStop && (!open || !sameStop(st, stop)) && !time.UnixMilli(stop.At).Before(since):
			d.record(ctx, s, stop)
		case open && !atStop:
			if running, err := d.api.Running(ctx, s.ID); err == nil && !running {
				d.checkWentOn(ctx, st)
			}
		}
	}
}

func (d *Daemon) record(ctx context.Context, sess opencode.Session, stop opencode.LimitStop) {
	now := d.cfg.Now()
	stoppedAt := time.UnixMilli(stop.At)
	lim := state.Limit{SessionID: sess.ID, Title: sess.Title, Directory: sess.Location.Directory,
		StoppedAt: stoppedAt, MessageID: stop.MessageID, RetryAfter: stop.Limit.RetryAfter}
	st, isNew := d.st.RecordLimit(lim, now, func() reset.Plan {
		return reset.Initial(now, stoppedAt, stop.Limit.RetryAfter, d.exhausted(ctx))
	})
	switch {
	case isNew:
		d.cfg.Log.Printf("%s: limit stop recorded, %s", d.name(st), describePlan(st.Plan(), st.Attempts))
		d.showNotice(ctx, st)
	case st.Status == state.Failed:
		d.clearNotice(ctx, st)
		d.cfg.Log.Printf("%s: %s, giving up", d.name(st), st.Note)
	}
}

// showNotice puts (or updates) the line in the session's TUI that says when it continues.
func (d *Daemon) showNotice(ctx context.Context, st *state.Stop) {
	if d.cfg.NoNotice || d.cfg.DryRun || d.api == nil || (st.NoticeID != "" && st.NoticeFor.Equal(st.NextAt)) {
		return
	}
	d.clearNotice(ctx, st)
	when := st.NextAt.Local().Format("Mon 15:04")
	desc := "LetsGoAgain · Claude limit reached · continues automatically " + when
	if st.Source == reset.SourceProbe {
		desc = "LetsGoAgain · Claude limit reached · reset unknown, checking again " + when
	}
	text := "Note from LetsGoAgain: this session stopped at the Claude usage limit and was set to continue automatically after the reset (" + when + ")."
	id, err := d.api.AddNotice(ctx, st.SessionID, desc, text)
	if err != nil {
		d.cfg.Log.Printf("%s: TUI notice not shown: %v", d.name(st), err)
		return
	}
	st.NoticeID, st.NoticeFor = id, st.NextAt
}

// clearNotice removes the line again, so the agent never receives it.
func (d *Daemon) clearNotice(ctx context.Context, st *state.Stop) {
	if st.NoticeID == "" || d.api == nil {
		return
	}
	if err := d.api.CancelNotice(ctx, st.SessionID, st.NoticeID); err != nil && !errors.Is(err, opencode.ErrNotFound) {
		d.cfg.Log.Printf("%s: TUI notice not removed: %v", d.name(st), err)
	}
	st.NoticeID, st.NoticeFor = "", time.Time{}
}

func (d *Daemon) exhausted(ctx context.Context) reset.Exhausted {
	q, err := d.cfg.Quota.Quota(ctx)
	if err != nil {
		d.cfg.Log.Printf("meridian quota unavailable: %v", err)
		return reset.Exhausted{}
	}
	at, window, spent := q.Exhausted(d.cfg.Now())
	return reset.Exhausted{Known: true, Spent: spent, ResetAt: at, Window: window}
}

func (d *Daemon) due(ctx context.Context) {
	now := d.cfg.Now()
	for _, st := range d.st.Sorted() {
		if st.Status != state.Waiting || st.NextAt.After(now) {
			continue
		}
		if d.api == nil {
			return // the service may be restarting: the stop stays due
		}
		next, resume := reset.Due(now, st.StoppedAt, st.Plan(), d.exhausted(ctx))
		if !resume {
			st.SetPlan(next)
			st.UpdatedAt = now
			d.cfg.Log.Printf("%s: still limited, %s", d.name(st), describePlan(next, st.Attempts))
			d.showNotice(ctx, st)
			continue
		}
		d.resume(ctx, st, "reset reached")
	}
}

// resume sends the resume message if the session is still exactly as the limit left it.
func (d *Daemon) resume(ctx context.Context, st *state.Stop, why string) error {
	now := d.cfg.Now()
	sess, err := d.api.Session(ctx, st.SessionID)
	if errors.Is(err, opencode.ErrNotFound) {
		st.Close(state.Gone, "session no longer exists", now)
		st.NoticeID, st.NoticeFor = "", time.Time{}
		d.cfg.Log.Printf("%s: session gone, stop closed", d.name(st))
		return err
	}
	if err != nil {
		return d.retryLater(st, err)
	}
	running, err := d.api.Running(ctx, st.SessionID)
	if err != nil {
		return d.retryLater(st, err)
	}
	msgs, err := d.api.RecentMessages(ctx, st.SessionID, 10)
	if err != nil {
		return d.retryLater(st, err)
	}
	stop, atStop := opencode.StopOf(msgs)
	if running || !atStop {
		st.Close(state.MovedOn, "session changed since the stop", now)
		d.clearNotice(ctx, st)
		d.cfg.Log.Printf("%s: skipped, the session moved on since the stop", d.name(st))
		return errors.New("session moved on since the stop")
	}
	if !sameStop(st, stop) {
		// It went on and stopped at the limit again without us seeing it: follow the newer stop.
		d.record(ctx, sess, stop)
		return errors.New("stopped at the limit again since; rescheduled")
	}
	if d.cfg.DryRun {
		st.Close(state.DryRun, "would have resumed ("+why+")", now)
		d.cfg.Log.Printf("%s: dry run, would resume now (%s)", d.name(st), why)
		return nil
	}
	// Before the prompt: a queued notice would otherwise be delivered to the agent along with it.
	d.clearNotice(ctx, st)
	if err := d.api.Prompt(ctx, st.SessionID, d.cfg.Message); err != nil {
		return d.retryLater(st, err)
	}
	st.MarkResumed(now)
	d.cfg.Log.Printf("%s: resume sent (%s, attempt %d/%d)", d.name(st), why, st.Attempts, state.MaxAttempts)
	return nil
}

func (d *Daemon) retryLater(st *state.Stop, err error) error {
	now := d.cfg.Now()
	st.NextAt, st.Note, st.UpdatedAt = now.Add(d.cfg.Reconnect), err.Error(), now
	d.cfg.Log.Printf("%s: could not resume, trying again shortly: %v", d.name(st), err)
	return err
}

func (d *Daemon) save() {
	if err := d.st.Save(d.cfg.StatePath, d.cfg.Now()); err != nil {
		d.cfg.Log.Printf("saving state: %v", err)
	}
}

func (d *Daemon) openCount() int {
	n := 0
	for _, st := range d.st.Stops {
		if st.Open() {
			n++
		}
	}
	return n
}

func (d *Daemon) name(st *state.Stop) string {
	if st.Title == "" {
		return st.SessionID
	}
	return fmt.Sprintf("%q (%s)", st.Title, st.SessionID)
}

func describePlan(p reset.Plan, attempts int) string {
	what := map[string]string{
		reset.SourceQuota:      "reset from the meridian quota",
		reset.SourceRetryAfter: "reset from retry_after",
		reset.SourceProbe:      "reset unknown, checking again",
	}[p.Source]
	if p.Window != "" {
		what += " (" + p.Window + ")"
	}
	s := fmt.Sprintf("%s at %s", what, p.At.Local().Format("Mon 15:04"))
	if attempts > 0 {
		s += fmt.Sprintf(", %d resumes so far", attempts)
	}
	return s
}
