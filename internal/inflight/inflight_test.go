package inflight

import (
	"os"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

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

// fabricate stubs the ancestry: names[i] is the process at pid 100+i, whose parent is the next one up.
func fabricate(t *testing.T, names []string, end int) {
	t.Helper()
	g, c, p := sessionGetppid, sessionComm, sessionParent
	t.Cleanup(func() { sessionGetppid, sessionComm, sessionParent = g, c, p })
	sessionGetppid = func() int { return 100 }
	sessionComm = func(pid int) string {
		if pid < 100 || pid-100 >= len(names) {
			return ""
		}
		return names[pid-100]
	}
	sessionParent = func(pid int) int {
		if pid-100 == len(names)-1 {
			return end
		}
		return pid + 1
	}
}

func TestSessionOwnerThroughWrappers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		chain []string
		end   int
		want  int
	}{
		{"direct", []string{"claude"}, 1, 100},
		{"rtk", []string{"rtk", "claude"}, 1, 101},
		{"timeout", []string{"timeout", "claude"}, 1, 101},
		{"env", []string{"env", "claude"}, 1, 101},
		{"sudo", []string{"sudo", "claude"}, 1, 101},
		{"nohup", []string{"nohup", "claude"}, 1, 101},
		{"time", []string{"time", "claude"}, 1, 101},
		{"nice", []string{"nice", "claude"}, 1, 101},
		{"stdbuf", []string{"stdbuf", "claude"}, 1, 101},
		{"xargs", []string{"xargs", "claude"}, 1, 101},
		{"make", []string{"make", "bash", "claude"}, 1, 102},
		{"go run", []string{"go", "zsh", "claude"}, 1, 102},
		{"dashed zsh", []string{"-zsh", "claude"}, 1, 101},
		{"dashed bash", []string{"-bash", "claude"}, 1, 101},
		{"fish", []string{"fish", "claude"}, 1, 101},
		{"ksh", []string{"ksh", "claude"}, 1, 101},
		{"stacked", []string{"rtk", "timeout", "sh", "claude"}, 1, 103},
		{"node cli", []string{"rtk", "node"}, 1, 101},
		{"no agent cli falls back", []string{"rtk", "sh", "tmux"}, 1, 102},
		{"wrapper is top", []string{"env", "rtk"}, 1, 0},
		{"wrapper top with live parent", []string{"env"}, 50, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fabricate(t, tt.chain, tt.end)
			if got := SessionOwner(); got != tt.want {
				t.Errorf("SessionOwner() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSessionOwnerWrapperBound(t *testing.T) {
	chain := slices.Repeat([]string{"rtk"}, 9)
	fabricate(t, append(chain, "claude"), 1)
	if got := SessionOwner(); got != 0 {
		t.Errorf("9 wrappers: SessionOwner() = %d, want 0 (bound)", got)
	}
	fabricate(t, append(chain[:8:8], "claude"), 1)
	if got := SessionOwner(); got != 108 {
		t.Errorf("8 wrappers: SessionOwner() = %d, want 108", got)
	}
}

func BenchmarkSessionOwner(b *testing.B) {
	g, c, p := sessionGetppid, sessionComm, sessionParent
	b.Cleanup(func() { sessionGetppid, sessionComm, sessionParent = g, c, p })
	names := []string{"rtk", "timeout", "-zsh", "claude"}
	sessionGetppid = func() int { return 100 }
	sessionComm = func(pid int) string { return names[pid-100] }
	sessionParent = func(pid int) int { return pid + 1 }
	for b.Loop() {
		SessionOwner()
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
