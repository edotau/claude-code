package inflight

import "testing"

func TestSessionOwner(t *testing.T) {
	originalGetppid, originalComm, originalParent := sessionGetppid, sessionComm, sessionParent
	t.Cleanup(func() {
		sessionGetppid, sessionComm, sessionParent = originalGetppid, originalComm, originalParent
	})
	for _, tt := range []struct {
		name   string
		parent int
		shells int
		end    int
		want   int
	}{
		{name: "reparented to init", parent: 1},
		{name: "shell chain ending at init", parent: 20, shells: 2, end: 1},
		{name: "shell chain exceeds limit", parent: 20, shells: 9, end: 42},
		{name: "unknown parent", parent: 0},
		{name: "negative parent", parent: -1},
		{name: "unreadable shell parent", parent: 20, shells: 1},
		{name: "session process", parent: 42, want: 42},
		{name: "session behind shells", parent: 20, shells: 2, end: 42, want: 42},
		{name: "session at limit", parent: 20, shells: 8, end: 42, want: 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sessionGetppid = func() int { return tt.parent }
			sessionComm = func(pid int) string {
				if pid >= 20 && pid < 20+tt.shells {
					return "sh"
				}
				return "claude"
			}
			sessionParent = func(pid int) int {
				if pid == 20+tt.shells-1 {
					return tt.end
				}
				return pid + 1
			}
			if got := SessionOwner(); got != tt.want {
				t.Errorf("SessionOwner() = %d, want %d", got, tt.want)
			}
		})
	}
}
