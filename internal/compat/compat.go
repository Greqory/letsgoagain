// Package compat knows which OpenCode and meridian versions LetsGoAgain was measured against
// (AGENTS.md §2) and says when a running one differs: an update there is the likeliest way for a limit
// stop to go unnoticed.
package compat

import (
	"fmt"
	"strconv"
	"strings"
)

// Tested major.minor versions. Raise them after `tools/sandbox/e2e.py` passes against a new OpenCode, or
// a real limit stop worked with a new meridian.
const (
	TestedOpenCode = "2.3"
	TestedMeridian = "1.79"
)

// Check compares a running version with the tested major.minor. ok is false when the running version is
// unknown or its major.minor differs; note says so in one line.
func Check(name, running, tested string) (note string, ok bool) {
	if running == "" {
		return fmt.Sprintf("%s version unknown (tested: %s)", name, tested), false
	}
	r, errR := majorMinor(running)
	t, errT := majorMinor(tested)
	if errR != nil || errT != nil {
		return fmt.Sprintf("%s %s: version not readable (tested: %s)", name, running, tested), false
	}
	switch {
	case r == t:
		return fmt.Sprintf("%s %s (tested: %s)", name, running, tested), true
	case r[0] > t[0] || (r[0] == t[0] && r[1] > t[1]):
		return fmt.Sprintf("%s %s is newer than the tested %s: if a limit stop is missed, please report it", name, running, tested), false
	default:
		return fmt.Sprintf("%s %s is older than the tested %s: consider updating it", name, running, tested), false
	}
}

func majorMinor(v string) ([2]int, error) {
	var out [2]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return out, fmt.Errorf("not major.minor: %q", v)
	}
	for i := range out {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, err
		}
		out[i] = n
	}
	return out, nil
}
