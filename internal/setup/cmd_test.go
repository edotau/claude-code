package setup

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

func TestMain(m *testing.M) {
	worktree.StubProcessScanForTest()
	agent.NoticeOut = io.Discard
	os.Exit(hosttest.Main(m))
}

// Run's bare help exits 0, an unknown mode 1, and adopt without --from is a usage error (2) that names --from.
func TestCmdSetupRunExits(t *testing.T) {
	for _, c := range []struct {
		args       []string
		want       int
		wantStderr string
	}{{nil, 0, ""}, {[]string{"bogus"}, 1, "bogus"}, {[]string{"adopt"}, 2, "--from"}} {
		got, _, stderr := hosttest.CaptureOutput(t, func() int { return run(c.args) })
		if got != c.want || !strings.Contains(stderr, c.wantStderr) {
			t.Errorf("Run(%v) = %d, stderr %q; want %d naming %q", c.args, got, stderr, c.want, c.wantStderr)
		}
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
	stdinFrom(t, dir+" extra-token\n")

	code, _, _ := hosttest.CaptureOutput(t, func() int {
		return cmdSetupNuke([]string{"--dir", dir, "--apply"})
	})
	if code == 0 {
		t.Error("partially scanned confirmation exit = 0, want refusal")
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
		{[]string{"status", "--with", "x", t.TempDir()}, 2}, // --with is init's
		{[]string{"status", t.TempDir(), t.TempDir()}, 2},   // at most one <root>
		{[]string{"status", t.TempDir()}, 1},                // not a git toplevel
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
// usage error (2), caught before workspaceLink ever runs — no symlink is created.
func TestSetupWorkspaceLinkRejectsDryRunWithoutLinking(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupWorkspaceGitRoot(t)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
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

// TestSetupWorkspaceInitDryRunWritesNothing: init --dry-run reaches the engine as a dry run: exit 0, nothing written.
func TestSetupWorkspaceInitDryRunWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := setupWorkspaceGitRoot(t)
	t.Chdir(root)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdSetupWorkspace([]string{"init", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("init --dry-run: code = %d, want 0; stderr=%q", code, stderr)
	}
	if _, err := os.Lstat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Errorf("init --dry-run wrote %s, stat err=%v", filepath.Join(root, ".claude"), err)
	}
}

// TestCmdSetupNukeAcceptsConfirmationWithSpaces: the whole typed line is the confirmation, so a path with a space works.
func TestCmdSetupNukeAcceptsConfirmationWithSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "my harness")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stdinFrom(t, dir+"\n")

	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdSetupNuke([]string{"--dir", dir, "--apply"})
	})
	if code != 0 {
		t.Fatalf("space-path confirmation exit = %d, want 0; out=%q stderr=%q", code, out, stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("confirmed nuke left %s behind, stat err=%v", dir, err)
	}
}

// TestSetupWorkspaceHelpListsInitFlagsOnly: link and status -h must not advertise --with/--dry-run, which they reject.
func TestSetupWorkspaceHelpListsInitFlagsOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for op, wantFlags := range map[string]bool{"init": true, "link": false, "status": false} {
		_, out, errOut := hosttest.CaptureOutput(t, func() int { return cmdSetupWorkspace([]string{op, "-h"}) })
		got := out + errOut
		for _, flag := range []string{"--with", "--dry-run"} {
			if has := strings.Contains(got, flag); has != wantFlags {
				t.Errorf("%s -h mentions %s = %v, want %v:\n%s", op, flag, has, wantFlags, got)
			}
		}
	}
}

// stdinFrom points os.Stdin at a file holding text until the test ends.
func stdinFrom(t *testing.T, text string) {
	t.Helper()
	input := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(input, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(input)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}
