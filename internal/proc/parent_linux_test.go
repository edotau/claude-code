//go:build linux

package proc

import "testing"

func TestParseStatPPID(t *testing.T) {
	stat := "12345 (weird ) ( comm) S 6789 12345 12345 0 -1 4194304 0 0 0 0"
	if got := parseStatPPID(stat); got != 6789 {
		t.Errorf("parseStatPPID(%q) = %d, want 6789", stat, got)
	}
}
