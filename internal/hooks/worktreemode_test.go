package hooks

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/worktree"
)

// TestWorktreeModeAndBusy: on asks every session; off stays silent until a peer appears, then reports a changed set once.
func TestWorktreeModeAndBusy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	peers := []worktree.Proc{{PID: 4242, Comm: "claude"}}
	old := peerAgents
	peerAgents = func(string, []string) ([]worktree.Proc, error) { return peers, nil }
	defer func() { peerAgents = old }()
	in := &Input{SessionID: "s1", CWD: cwd}

	t.Setenv(worktreeModeEnv, "on")
	if got := worktreeMode(in).Context; !strings.HasPrefix(got, "Worktree mode on:") {
		t.Fatalf("on: %q", got)
	}
	t.Setenv(worktreeModeEnv, "")
	if got := worktreeMode(in).Context; !strings.Contains(got, "claude pid 4242") {
		t.Fatalf("off start: %q", got)
	}
	if got := worktreeBusy(in).Context; got != "" {
		t.Fatalf("same peers must dedupe, got %q", got)
	}
	peers = []worktree.Proc{{PID: 4242, Comm: "claude"}, {PID: 77, Comm: "codex"}}
	if got := worktreeBusy(in).Context; got == "" {
		t.Fatal("changed peer set must re-report")
	}
	peers = nil
	if got := worktreeBusy(in).Context; got != "" {
		t.Fatalf("no peers: %q", got)
	}
}
