package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scrubGitEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
	} {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
		})
	}
}

// gitRepoCLI makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepoCLI(t *testing.T) string {
	t.Helper()
	scrubGitEnv(t)
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "--allow-empty", "-q", "-m", "init")
	return dir
}

func TestCmdWorktreeNoArgsAndUnknownSub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	if got := cmdWorktree(nil); got != 0 {
		t.Fatalf("no args: got %d, want 0", got)
	}
	if got := cmdWorktree([]string{"bogus"}); got != 2 {
		t.Fatalf("unknown sub: got %d, want 2", got)
	}
}

func TestCmdWorktreeNonRepoCWD(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir()) // not a git repository
	code, _, errb := captureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit outside a git repo, stderr=%q", errb)
	}
}

func TestCmdWorktreeListEmptyTextAndJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "no worktrees under") {
		t.Fatalf("text list empty: code=%d out=%q", code, out)
	}

	code, out, _ = captureOutput(t, func() int { return cmdWorktree([]string{"list", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("json list empty: code=%d", code)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("invalid JSON: %v, out=%q", err, out)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
	}
}

func TestCmdWorktreeCreateThenListShowsIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"create", "feature"}) })
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("created worktree missing at %s: %v", dest, err)
	}

	code, out, _ = captureOutput(t, func() int { return cmdWorktree([]string{"list", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1: %v", len(entries), entries)
	}
}

func TestCmdWorktreeCreateWithCustomBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"create", "--branch", "wt/custom", "feature"}) })
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	branchOut, err := exec.Command("git", "-C", dest, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(branchOut)) != "wt/custom" {
		t.Fatalf("branch = %q, err %v, want wt/custom", branchOut, err)
	}
}

func TestCmdWorktreeCreateWrongArity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	for _, args := range [][]string{{"create"}, {"create", "a", "b"}} {
		code, _, errb := captureOutput(t, func() int { return cmdWorktree(append([]string{}, args...)) })
		if code == 0 {
			t.Errorf("args %v: want non-zero exit, stderr=%q", args, errb)
		}
	}
}

func TestCmdWorktreeCleanMissingName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	code, _, errb := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "does-not-exist"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit for a missing worktree, stderr=%q", errb)
	}
}

func TestCmdWorktreeCleanDryRunDoesNotRemove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "--dry-run", "mergeable"}) })
	if code != 0 || !strings.Contains(out, "eligible:") {
		t.Fatalf("dry-run clean: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("--dry-run must never remove the worktree")
	}
}

func TestCmdWorktreeCleanRealRemovesEligible(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "mergeable"}) })
	if code != 0 || !strings.Contains(out, "removed:") {
		t.Fatalf("real clean: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("worktree should be gone, stat err=%v", err)
	}
}

func TestCmdWorktreeCleanDirtyRefusedExitsNonZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"create", "dirty"}) })
	dest := strings.TrimSpace(out)
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean", "dirty"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit for a refused dirty clean, out=%q", out)
	}
	if !strings.Contains(out, "refused:") {
		t.Fatalf("out=%q, want a refused: line", out)
	}
}

// TestCmdWorktreeCleanAllOverlappingFlagsPreviewWins asserts --dry-run and --yes together must still
// preview, never remove — the safer of two conflicting flags has to win when both are given.
func TestCmdWorktreeCleanAllOverlappingFlagsPreviewWins(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := captureOutput(t, func() int {
		return cmdWorktree([]string{"clean-all", "--dry-run", "--yes"})
	})
	if code != 0 {
		t.Fatalf("clean-all --dry-run --yes: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "preview only") {
		t.Fatalf("out=%q, want the preview-only summary even with --yes also set", out)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("--dry-run combined with --yes must still never remove anything")
	}
}

func TestCmdWorktreeCleanAllRequiresYesToApply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := captureOutput(t, func() int { return cmdWorktree([]string{"clean-all"}) })
	if code != 0 {
		t.Fatalf("clean-all (no flags): code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "preview only") {
		t.Fatalf("out=%q, want a preview-only summary without --yes", out)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("clean-all without --yes must never remove anything")
	}
}

// TestCmdWorktreeListReopenCommandIsShellSafe is the regression test for Appendix A codex-review
// finding #7 (internal/cli/worktree.go:110): Go's %q uses double-quote syntax, which a POSIX shell
// still expands $(...) and `...` inside of — pasting the printed "reopen" command for a worktree
// named with a command substitution must never execute it.
func TestCmdWorktreeListReopenCommandIsShellSafe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	marker := "pwned-marker"
	name := "x$(touch${IFS}" + marker + ")y"
	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"create", "--branch", "wt/safe-branch", name})
	})
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("worktree missing at %s: %v", dest, err)
	}

	code, listOut, errb := captureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
	if code != 0 {
		t.Fatalf("list: code=%d stderr=%q", code, errb)
	}
	var reopenLine string
	for line := range strings.SplitSeq(listOut, "\n") {
		if strings.Contains(line, "reopen:") {
			reopenLine = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "reopen:"))
		}
	}
	if reopenLine == "" {
		t.Fatalf("no reopen line found in list output: %q", listOut)
	}

	run := exec.Command("sh", "-c", reopenLine)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("running the printed reopen command failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the printed reopen command executed the embedded command substitution: marker created at %s\nprinted command: %s", marker, reopenLine)
	}
}

func TestCmdWorktreeCreateUnusualName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"create", "dots.and-dashes_123"}) })
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(strings.TrimSpace(out)); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
}
