package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdSetupNoArgs(t *testing.T) {
	if got := cmdSetup(nil); got != 0 {
		t.Fatalf("no-args exit = %d, want 0", got)
	}
}

func TestCmdSetupUnknownMode(t *testing.T) {
	if got := cmdSetup([]string{"bogus"}); got != 1 {
		t.Fatalf("unknown mode exit = %d, want 1", got)
	}
}

func TestCmdSetupAdoptRequiresFrom(t *testing.T) {
	if got := cmdSetupAdopt(nil); got != 2 {
		t.Fatalf("missing --from exit = %d, want 2", got)
	}
}

func TestCmdSetupAdoptRefusesExistingGit(t *testing.T) {
	into := t.TempDir()
	if err := os.MkdirAll(filepath.Join(into, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	from := t.TempDir()
	if got := cmdSetupAdopt([]string{"--from", from, "--into", into}); got != 2 {
		t.Fatalf("existing .git exit = %d, want 2", got)
	}
}

func TestCmdSetupNukeApplyWithoutConfirmRefused(t *testing.T) {
	dir := t.TempDir()
	// --apply prompts for confirmation on stdin; an empty/closed stdin never matches dir.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}

	defer devnull.Close()
	oldStdin := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = oldStdin }()

	if got := cmdSetupNuke([]string{"--dir", dir, "--apply"}); got != 2 {
		t.Fatalf("apply without a matching confirm exit = %d, want 2", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dir must survive a refused nuke")
	}
}

func TestCmdSetupNukeRejectsPartialConfirmation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(home, "confirmation")
	if err := os.WriteFile(input, []byte(dir+" extra-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })

	code, out, _ := captureOutput(t, func() int {
		return cmdSetupNuke([]string{"--dir", dir, "--apply"})
	})
	if code == 0 {
		t.Error("partially scanned confirmation exit = 0, want refusal")
	}
	if strings.Contains(out, "nuke plan for") {
		t.Errorf("invalid confirmation reached Nuke: %q", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("target must survive invalid confirmation: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(home, "claude-nuke-*.tgz"))
	if err != nil || len(archives) != 0 {
		t.Errorf("invalid confirmation created archives: %v, %v", archives, err)
	}
}

// Bare help exits 0; arity errors exit 2 before filesystem work, and a bad root exits 1 from the engine.
func TestSetupWorkspaceUsageExits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cases := []struct {
		args []string
		want int
	}{
		{nil, 0},
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
