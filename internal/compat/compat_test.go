package compat

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		running string
		ok      bool
		note    string
	}{
		{"2.3.0", true, "OpenCode 2.3.0 (tested: 2.3)"},
		{"2.3.7", true, "OpenCode 2.3.7 (tested: 2.3)"},
		{"v2.3.1", true, "(tested: 2.3)"},
		{"2.4.0", false, "OpenCode 2.4.0 is newer than the tested 2.3"},
		{"3.0.0", false, "is newer"},
		{"2.10.0", false, "is newer"},
		{"2.2.9", false, "OpenCode 2.2.9 is older than the tested 2.3"},
		{"", false, "OpenCode version unknown (tested: 2.3)"},
		{"latest", false, "version not readable"},
	}
	for _, tt := range tests {
		note, ok := Check("OpenCode", tt.running, "2.3")
		if ok != tt.ok || !strings.Contains(note, tt.note) {
			t.Errorf("Check(%q) = %q, %v; want %v and %q", tt.running, note, ok, tt.ok, tt.note)
		}
	}
}
