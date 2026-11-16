package daemon

import (
	"context"
	"sort"
	"time"

	"github.com/Greqory/letsgoagain/internal/meridian"
	"github.com/Greqory/letsgoagain/internal/state"
)

// readTimeout bounds what a status request asks of the OpenCode service and meridian, so a hung one
// cannot hold up the daemon's loop for long.
const readTimeout = 10 * time.Second

// Listener is a running root session: if it hits the limit, the daemon records the stop and resumes it.
type Listener struct {
	SessionID string    `json:"sessionID"`
	Title     string    `json:"title,omitempty"`
	Since     time.Time `json:"since,omitempty"` // when its current run began, if known
}

// Overview is everything `letsgoagain status` shows that the daemon knows.
type Overview struct {
	Stops        []state.Stop    `json:"stops,omitempty"`
	Connected    bool            `json:"connected"` // the OpenCode service's event stream is up
	OpenCode     string          `json:"opencode,omitempty"`
	DryRun       bool            `json:"dryRun,omitempty"`
	Listening    []Listener      `json:"listening,omitempty"`
	ListeningErr string          `json:"listeningErr,omitempty"`
	Quota        *meridian.Quota `json:"quota,omitempty"`
	QuotaErr     string          `json:"quotaErr,omitempty"`
}

func (d *Daemon) overview(ctx context.Context) *Overview {
	ov := &Overview{Connected: d.api != nil, DryRun: d.cfg.DryRun}
	for _, st := range d.st.Sorted() {
		ov.Stops = append(ov.Stops, *st)
	}
	if d.api != nil {
		ov.OpenCode = d.api.Version()
		lctx, cancel := context.WithTimeout(ctx, readTimeout)
		ov.Listening, ov.ListeningErr = d.listening(lctx)
		cancel()
	}
	qctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if q, err := d.cfg.Quota.Quota(qctx); err != nil {
		ov.QuotaErr = err.Error()
	} else {
		ov.Quota = &q
	}
	return ov
}

// listening lists the running root sessions. Subagents are left out: when one hits the limit its root
// fails too and is the one resumed.
func (d *Daemon) listening(ctx context.Context) ([]Listener, string) {
	ids, err := d.api.Active(ctx)
	if err != nil {
		return nil, err.Error()
	}
	var out []Listener
	for _, id := range ids {
		s, err := d.api.Session(ctx, id)
		if err != nil || s.ParentID != "" {
			continue // finished meanwhile, or a subagent
		}
		l := Listener{SessionID: id, Title: s.Title}
		if s.Time.Busy > s.Time.Idle {
			l.Since = time.UnixMilli(s.Time.Busy)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, ""
}
