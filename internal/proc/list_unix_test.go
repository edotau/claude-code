//go:build unix

package proc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

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
