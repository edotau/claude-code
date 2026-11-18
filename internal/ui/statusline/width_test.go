package statusline

import (
	"errors"
	"testing"
)

func TestClipCountsWideCells(t *testing.T) {
	if got := visibleWidth("界界"); got != 4 {
		t.Errorf("wide text occupies %d columns, want 4", got)
	}
	if got := plain(clip("界界", 2)); got != "…" {
		t.Fatalf("clip wide text to 2 columns = %q, want ellipsis only", got)
	}
}

func TestWalkAncestors(t *testing.T) {
	type row struct {
		ppid int
		tty  string
		err  error
	}
	table := map[int]row{
		10: {ppid: 9, tty: "??"},
		9:  {ppid: 8, tty: "ttys001"},
		8:  {ppid: 7, tty: "ttys002"},
		7:  {ppid: 1, tty: "-"},
	}
	lookup := func(pid int) (int, string, error) {
		r, ok := table[pid]
		if !ok {
			return 0, "", errors.New("gone")
		}
		return r.ppid, r.tty, r.err
	}
	widths := map[string]int{"/dev/ttys001": 120, "/dev/ttys002": 200}
	cols := func(dev string) int { return widths[dev] }
	for _, tc := range []struct {
		name string
		pid  int
		want int
	}{
		{"width at the parent", 9, 120},
		{"width at a grandparent", 10, 120},
		{"first readable tty wins", 8, 200},
		{"no tty anywhere", 7, 0},
		{"lookup error mid-walk", 11, 0},
	} {
		if got := walkAncestors(tc.pid, lookup, cols); got != tc.want {
			t.Errorf("%s: walkAncestors(%d) = %d, want %d", tc.name, tc.pid, got, tc.want)
		}
	}
	table[9] = row{ppid: 8, tty: "ttys009"} // a tty that reports no width is passed over
	if got := walkAncestors(10, lookup, cols); got != 200 {
		t.Errorf("unreadable tty: got %d, want the next ancestor's 200", got)
	}
	table[8] = row{err: errors.New("denied")}
	if got := walkAncestors(10, lookup, cols); got != 0 {
		t.Errorf("error after a skipped tty: got %d, want 0", got)
	}
}
