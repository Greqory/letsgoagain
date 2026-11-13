// Package reset decides when a stopped session may continue: from meridian's quota, from the error's
// retry_after, or by probing (AGENTS.md §3 "Reset time, in order of trust").
package reset

import "time"

// Sources of a planned time.
const (
	SourceQuota      = "quota"
	SourceRetryAfter = "retry_after"
	SourceProbe      = "probe"
)

const (
	// Margin is added to a known reset before resuming.
	Margin = 60 * time.Second
	// ProbeInterval is the wait between quota checks when no reset time is known.
	ProbeInterval = 15 * time.Minute
	// ProbeCeiling is how long after a stop probing goes on before one resume is tried anyway.
	ProbeCeiling = 5 * time.Hour
	// retry_after values meridian uses when it does not know the reset (60) or clamped it (86400).
	retryAfterUnknown = 60
	retryAfterClamped = 86400
)

// Exhausted is what the quota says: the reset of the spent window, if one is spent.
type Exhausted struct {
	Known   bool // the quota could be read
	Spent   bool
	ResetAt time.Time
	Window  string
}

// Plan is when to look at a stop next.
type Plan struct {
	At     time.Time
	Source string
	Window string
}

// Initial plans a fresh stop.
func Initial(now, stoppedAt time.Time, retryAfter int, q Exhausted) Plan {
	if q.Known && q.Spent {
		return Plan{At: q.ResetAt.Add(Margin), Source: SourceQuota, Window: q.Window}
	}
	if retryAfter > retryAfterUnknown && retryAfter < retryAfterClamped {
		return Plan{At: stoppedAt.Add(time.Duration(retryAfter)*time.Second + Margin), Source: SourceRetryAfter}
	}
	return probe(now, stoppedAt)
}

// Due decides about a stop whose planned time has come: resume now (ok) or the next plan.
func Due(now, stoppedAt time.Time, current Plan, q Exhausted) (next Plan, resume bool) {
	if q.Known && q.Spent {
		return Plan{At: q.ResetAt.Add(Margin), Source: SourceQuota, Window: q.Window}, false
	}
	if current.Source != SourceProbe || q.Known || !now.Before(stoppedAt.Add(ProbeCeiling)) {
		return current, true
	}
	return probe(now, stoppedAt), false
}

func probe(now, stoppedAt time.Time) Plan {
	at := now.Add(ProbeInterval)
	if ceiling := stoppedAt.Add(ProbeCeiling); at.After(ceiling) {
		at = ceiling
	}
	return Plan{At: at, Source: SourceProbe}
}
