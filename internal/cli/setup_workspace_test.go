package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Usage and arity errors exit 2 before any filesystem work; a bad root exits 1 from the engine.
func TestSetupWorkspaceUsageExits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"bogus"}, 2},
		{[]string{"link", "agents"}, 2},
		{[]string{"init", "a", "b"}, 2},
		{[]string{"init", "--with", "x"}, 2},
		{[]string{"status", t.TempDir()}, 1}, // not a git toplevel
	}
	for _, c := range cases {
		if got := cmdSetupWorkspace(c.args); got != c.want {
			t.Errorf("cmdSetupWorkspace(%v) = %d, want %d", c.args, got, c.want)
		}
	}
}

// setupWorkspaceGitRoot makes a fresh git toplevel outside ~/.claude for init/status to validate against.
func setupWorkspaceGitRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	return root
}

// TestSetupWorkspaceLinkRejectsDryRunWithoutLinking: --dry-run only applies to init; on link it is a
// usage error (2), caught before WorkspaceLink ever runs — no symlink is created.
func TestSetupWorkspaceLinkRejectsDryRunWithoutLinking(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupWorkspaceGitRoot(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdSetupWorkspace([]string{"link", "agents", "demo", root, "--dry-run"})
	})
	if code != 2 {
		t.Fatalf("link --dry-run: code = %d, want 2; stderr=%q", code, stderr)
	}
	dst := filepath.Join(root, ".claude", "agents", "demo.md")
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("link --dry-run created a symlink at %s, stat err=%v", dst, err)
	}
}

// TestSetupWorkspaceStatusRejectsWith: --with only applies to init; on status it is a usage error (2).
func TestSetupWorkspaceStatusRejectsWith(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupWorkspaceGitRoot(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdSetupWorkspace([]string{"status", "--with", "x", root})
	})
	if code != 2 {
		t.Fatalf("status --with x: code = %d, want 2; stderr=%q", code, stderr)
	}
}

// TestSetupWorkspaceInitDryRunSucceeds: init --dry-run on a real git toplevel outside the config dir
// exits 0 and only previews — the full write path is covered at the internal/setup layer.
func TestSetupWorkspaceInitDryRunSucceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupWorkspaceGitRoot(t)
	t.Chdir(root)
	code, _, stderr := captureOutput(t, func() int {
		return cmdSetupWorkspace([]string{"init", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("init --dry-run: code = %d, want 0; stderr=%q", code, stderr)
	}
}
