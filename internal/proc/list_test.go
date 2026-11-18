package proc

import (
	"context"
	"errors"
	"slices"
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
	got := parseList(out, nil, DispatchIDEnv, time.UTC)
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

// Given a command whose executable path holds a space, When comm supplies the path, Then Exe is the whole base name.
func TestParseListExeWithSpace(t *testing.T) {
	out := "  88 1 88 Wed Nov 11 11:17:11 2026 /Applications/Visual Studio Code.app/Contents/Code Helper --type=gpu HOME=/x\n"
	comms := parseComms("   88 /Applications/Visual Studio Code.app/Contents/Code Helper\n  junk\n")
	got := parseList(out, comms, DispatchIDEnv, time.UTC)
	if len(got) != 1 || got[0].Exe != "Code Helper" {
		t.Fatalf("got %+v, want one row with Exe %q", got, "Code Helper")
	}
}

// Given recorded ps output where one pid exits between the calls, Then listWith equals the sequential parse.
func TestListWithMatchesSequentialParse(t *testing.T) {
	list := "  10 1 10 Wed Nov 11 11:17:11 2026 /bin/a x HARNESS_DISPATCH_ID=m1\n" +
		"  11 1 11 Wed Nov 11 11:17:12 2026 /Applications/Code Helper --type=gpu\n" +
		"  12 1 12 Wed Nov 11 11:17:13 2026 /bin/gone\n"
	comm := "  10 /bin/a\n  11 /Applications/Code Helper\n  99 /bin/exited\n"
	ps := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "-axo" {
			return comm, nil
		}
		return list, nil
	}
	got, err := listWith(ps, DispatchIDEnv)
	want := parseList(list, parseComms(comm), DispatchIDEnv, time.Local)
	if err != nil || !slices.Equal(got, want) || len(got) != 3 || got[1].Exe != "Code Helper" || got[2].Exe != "gone" {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	boom := errors.New("boom")
	failing := func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "-axo" {
			return "", errors.New("comm")
		}
		return "", boom
	}
	if _, err := listWith(failing, ""); err != boom {
		t.Fatalf("err = %v, want the first call's error", err)
	}
}
