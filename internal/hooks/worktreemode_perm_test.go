package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestWorktreeBusyStateFileIs0600: the busy-peer state file is private to the owner.
func TestWorktreeBusyStateFileIs0600(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "")
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	old := peerAgents
	peerAgents = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	defer func() { peerAgents = old }()
	worktreeBusy(&Input{SessionID: "s1", CWD: cwd})
	files, _ := filepath.Glob(filepath.Join(paths.StateDir(), "worktree-busy", "*"))
	if len(files) != 1 {
		t.Fatalf("want one state file, got %v", files)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
}
