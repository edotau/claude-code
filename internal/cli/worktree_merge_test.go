package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/worktree"
)

// mergeFixtureCLIWrite writes content to file in dir and commits it with msg, with the test identity
// also set into local repo config so the CLI's own git invocations (not just this helper's) resolve
// a committer for any merge commit worktree merge creates.
func mergeFixtureCLIWrite(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureCLIRun(t, dir, "add", file)
	mergeFixtureCLIRun(t, dir, "commit", "-q", "-m", msg)
}

func mergeFixtureCLIRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// mergeFixtureCLISetup makes a repo + a "feature" worktree via the CLI's own create path, with
// local git identity configured so a later `worktree merge` apply can create a merge commit.
func mergeFixtureCLISetup(t *testing.T) (repo string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo = gitRepoCLI(t)
	mergeFixtureCLIRun(t, repo, "config", "user.email", "t@t.com")
	mergeFixtureCLIRun(t, repo, "config", "user.name", "t")
	t.Chdir(repo)
	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"create", "feature"}) })
	if code != 0 {
		t.Fatalf("create feature: code=%d stderr=%q", code, errb)
	}
	wt := strings.TrimSpace(out)
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree missing at %s: %v", wt, err)
	}
	return repo
}

// mergeFixtureCLIWorktreePath resolves the "feature" worktree's path the same way the CLI itself
// does (worktree.Root), rather than re-deriving ~/worktrees/<name> by hand.
func mergeFixtureCLIWorktreePath(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Join(worktree.Root(repo), "feature")
}

func cliHeadOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCmdWorktreeMergeDryRunCleanShowsPlanWithoutMutating(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "--dry-run", "feature"}) })
	if code != 0 {
		t.Fatalf("merge --dry-run: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "clean") {
		t.Fatalf("output = %q, want a clean-plan message", out)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatalf("main HEAD changed under --dry-run")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dry-run must not remove the worktree: %v", err)
	}
}

func TestCmdWorktreeMergeApplyPromotesAndAutoCleans(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceOID := cliHeadOf(t, wt)

	// The fixture repo has no detectable gate (no Makefile/go.mod/etc) — --no-verify isolates this
	// test's intent (promote + auto-clean) from the separate verify-by-default behavior.
	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "--no-verify", "feature"}) })
	if code != 0 {
		t.Fatalf("merge apply: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree still on disk after a successful merge without --no-clean")
	}
}

func TestCmdWorktreeMergeApplyNoCleanLeavesWorktree(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "--no-verify", "--no-clean", "feature"}) })
	if code != 0 {
		t.Fatalf("merge --no-clean: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("--no-clean must leave the worktree on disk: %v", err)
	}
}

// TestCmdWorktreeMergeVerifyFailurePreservesWorktreeAndSkipsPromotion is the regression test for
// Appendix A codex-review finding #1 (internal/cli/worktree.go:191): promotion and automatic cleanup
// must not happen without a scoped verification step — a failing --verify command must stop before
// Promote ever runs and leave the worktree on disk for inspection.
func TestCmdWorktreeMergeVerifyFailurePreservesWorktreeAndSkipsPromotion(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--verify", "exit 1", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit when --verify fails, stdout=%q stderr=%q", out, errb)
	}
	if strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, must never reach promotion after a failed verify", out)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a failed --verify — promotion must be skipped entirely")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must be preserved after a failed --verify, for inspection: %v", err)
	}
}

// TestCmdWorktreeMergeVerifySuccessRunsInsideWorktreeThenPromotes covers the success half: --verify
// runs inside the worktree (not main) after integration, and a zero exit lets promotion proceed.
func TestCmdWorktreeMergeVerifySuccessRunsInsideWorktreeThenPromotes(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceOID := cliHeadOf(t, wt)

	verifyCmd := `test -f g.txt && [ "$(pwd -P)" = "` + wt + `" ]`
	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--verify", verifyCmd, "feature"})
	})
	if code != 0 {
		t.Fatalf("merge --verify (passing): code=%d stdout=%q stderr=%q", code, out, errb)
	}
	if !strings.Contains(out, "verify: passed") {
		t.Fatalf("output = %q, want a verify-passed line", out)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line after a passing verify", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
}

func TestCmdWorktreeMergeConflictBlocksAndLeavesMainUntouched(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "feature"}) })
	if code == 0 {
		t.Fatalf("merge on predicted conflict: want non-zero exit, stdout=%q stderr=%q", out, errb)
	}
	if !strings.Contains(out, "conflict") {
		t.Fatalf("output = %q, want the conflict issue reported", out)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a blocked merge")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a blocked merge: %v", err)
	}
}

func TestCmdWorktreeMergeUnknownNameFails(t *testing.T) {
	mergeFixtureCLISetup(t)
	code, _, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "does-not-exist"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit for an unknown worktree name, stderr=%q", errb)
	}
}

func TestCmdWorktreeMergeTargetMismatchFails(t *testing.T) {
	mergeFixtureCLISetup(t)
	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--target", "some-other-branch", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit on target mismatch, stdout=%q stderr=%q", out, errb)
	}
	if !strings.Contains(out, "target-mismatch") {
		t.Fatalf("output = %q, want target-mismatch reported", out)
	}
}

func TestCmdWorktreeMergeNoArgsFails(t *testing.T) {
	mergeFixtureCLISetup(t)
	code, _, _ := captureOutput(t, func() int { return cmdWorktree([]string{"merge"}) })
	if code == 0 {
		t.Fatal("want non-zero exit for merge with no name")
	}
}
