package proc

import (
	"testing"
	"time"
)

// Given BSD and GNU ps rows (padded lstart, env after the command), Then parseList reads ids, start, exe and the mark.
func TestParseListReadsMarkAndStart(t *testing.T) {
	out := "63010     1 63010 Wed Nov 11 11:17:11 2026     /Users/edotau/.claude/bin/opencode run --format json " +
		"HOME=/Users/edotau HARNESS_DISPATCH_ID=20261111T161710.655-opencode PWD=/x\n" +
		"  412   400   412 Mon Nov  9 09:05:01 2026 /usr/bin/ssh -v host SSH_AUTH_SOCK=/tmp/a\n" +
		"garbage line\n" +
		"  7 1 7 Wed Nov 11 xx:17:11 2026 /bad/time\n"
	got := parseList(out, DispatchIDEnv, time.UTC)
	if len(got) != 2 {
		t.Fatalf("parsed %d rows, want 2 (bad rows skipped): %+v", len(got), got)
	}
	want := Info{PID: 63010, PPID: 1, PGID: 63010, Start: time.Date(2026, 11, 11, 11, 17, 11, 0, time.UTC),
		Exe: "opencode", Mark: "20261111T161710.655-opencode"}
	if got[0] != want {
		t.Errorf("row 0 = %+v, want %+v", got[0], want)
	}
	if got[1].Mark != "" || got[1].Exe != "ssh" || got[1].Start.Day() != 9 {
		t.Errorf("row 1 = %+v, want an unmarked ssh started on the 9th", got[1])
	}
}
