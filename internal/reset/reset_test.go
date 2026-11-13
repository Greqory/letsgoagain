package reset

import (
	"testing"
	"time"
)

var (
	stop = time.Date(2026, 11, 13, 1, 37, 0, 0, time.UTC)
	now  = stop.Add(2 * time.Second)
)

func TestInitial(t *testing.T) {
	quotaReset := stop.Add(81 * time.Minute)
	tests := []struct {
		name       string
		retryAfter int
		q          Exhausted
		want       Plan
	}{
		{"quota spent wins over retry_after", 4868, Exhausted{Known: true, Spent: true, ResetAt: quotaReset, Window: "five_hour"},
			Plan{At: quotaReset.Add(Margin), Source: SourceQuota, Window: "five_hour"}},
		{"retry_after when quota says nothing spent", 4868, Exhausted{Known: true},
			Plan{At: stop.Add(4868*time.Second + Margin), Source: SourceRetryAfter}},
		{"retry_after when quota unreachable", 4868, Exhausted{},
			Plan{At: stop.Add(4868*time.Second + Margin), Source: SourceRetryAfter}},
		{"60 is meridian's unknown: probe", 60, Exhausted{}, Plan{At: now.Add(ProbeInterval), Source: SourceProbe}},
		{"86400 is clamped: probe", 86400, Exhausted{}, Plan{At: now.Add(ProbeInterval), Source: SourceProbe}},
		{"no retry_after: probe", 0, Exhausted{Known: true}, Plan{At: now.Add(ProbeInterval), Source: SourceProbe}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Initial(now, stop, tt.retryAfter, tt.q); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProbeStopsAtCeiling(t *testing.T) {
	late := stop.Add(ProbeCeiling - 5*time.Minute)
	if got := Initial(late, stop, 0, Exhausted{}); !got.At.Equal(stop.Add(ProbeCeiling)) {
		t.Fatalf("got %v, want the ceiling", got.At)
	}
}

func TestDue(t *testing.T) {
	later := stop.Add(2 * time.Hour)
	weekly := stop.Add(49 * time.Hour)
	known := Plan{At: later, Source: SourceRetryAfter}
	probing := Plan{At: later, Source: SourceProbe}
	tests := []struct {
		name   string
		now    time.Time
		plan   Plan
		q      Exhausted
		resume bool
		want   Plan
	}{
		{"still spent: follow the quota", later, known, Exhausted{Known: true, Spent: true, ResetAt: weekly, Window: "seven_day"}, false,
			Plan{At: weekly.Add(Margin), Source: SourceQuota, Window: "seven_day"}},
		{"quota clear: resume", later, known, Exhausted{Known: true}, true, known},
		{"quota unreachable, time known: resume", later, known, Exhausted{}, true, known},
		{"probe, quota clear: resume", later, probing, Exhausted{Known: true}, true, probing},
		{"probe, quota unreachable: probe again", later, probing, Exhausted{}, false, Plan{At: later.Add(ProbeInterval), Source: SourceProbe}},
		{"probe, quota unreachable at the ceiling: resume", stop.Add(ProbeCeiling), probing, Exhausted{}, true, probing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, resume := Due(tt.now, stop, tt.plan, tt.q)
			if resume != tt.resume || next != tt.want {
				t.Fatalf("got %+v resume=%v; want %+v resume=%v", next, resume, tt.want, tt.resume)
			}
		})
	}
}
