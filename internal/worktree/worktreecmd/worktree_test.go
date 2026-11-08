package worktreecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/worktree"
)

func TestCmdWorktreeNoArgsAndUnknownSub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	if got := Run(nil); got != 0 {
		t.Fatalf("no args: got %d, want 0", got)
	}
	if got := Run([]string{"bogus"}); got != 2 {
		t.Fatalf("unknown sub: got %d, want 2", got)
	}
}

func TestCmdWorktreeNonRepoCWD(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir()) // not a git repository
	code, _, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"list"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit outside a git repo, stderr=%q", errb)
	}
}

func TestCmdWorktreeListEmptyTextAndJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "no worktrees under") {
		t.Fatalf("text list empty: code=%d out=%q", code, out)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return Run([]string{"list", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("json list empty: code=%d", code)
	}
	if strings.TrimSpace(out) == "null" {
		t.Fatalf("empty list printed null, want []")
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

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "feature"}) })
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("created worktree missing at %s: %v", dest, err)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return Run([]string{"list", "--format", "json"}) })
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

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "--branch", "wt/custom", "feature"}) })
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
		code, _, errb := hosttest.CaptureOutput(t, func() int { return Run(append([]string{}, args...)) })
		if code == 0 {
			t.Errorf("args %v: want non-zero exit, stderr=%q", args, errb)
		}
	}
}

func TestCmdWorktreeCleanMissingName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	code, _, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "does-not-exist"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit for a missing worktree, stderr=%q", errb)
	}
}

func TestCmdWorktreeCleanDryRunDoesNotRemove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	_, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "--dry-run", "mergeable"}) })
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
	_, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "mergeable"}) })
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
	_, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "dirty"}) })
	dest := strings.TrimSpace(out)
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "dirty"}) })
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
	_, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"clean-all", "--dry-run", "--yes"})
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
	_, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "mergeable"}) })
	dest := strings.TrimSpace(out)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean-all"}) })
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
	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"create", "--branch", "wt/safe-branch", name})
	})
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("worktree missing at %s: %v", dest, err)
	}

	code, listOut, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"list"}) })
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
	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "dots.and-dashes_123"}) })
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(strings.TrimSpace(out)); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}
}

// forceFixtureDirty makes the "feature" worktree dirty (untracked file) and one commit ahead of main.
func forceFixtureDirty(t *testing.T) (repo, wt string) {
	t.Helper()
	repo = mergeFixtureCLISetup(t)
	wt = mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

func forceBranchList(t *testing.T, repo, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCmdWorktreeCleanForceRefusedWithoutFlag(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	code, _, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "feature"}) })
	if code == 0 {
		t.Fatal("plain clean of a dirty+unmerged worktree must fail")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a refused clean: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("branch must survive a refused clean")
	}
}

func TestCmdWorktreeCleanForceDiscardsDirtyUnmerged(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "--force", "feature"}) })
	if code != 0 {
		t.Fatalf("clean --force: code=%d stdout=%q stderr=%q", code, out, errb)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("worktree dir still exists after clean --force")
	}
	if got := forceBranchList(t, repo, "worktree-feature"); got != "" {
		t.Fatalf("branch should be deleted with -D, branch --list = %q", got)
	}
	// summary names the discarded work: branch, uncommitted file, and unmerged commit
	for _, want := range []string{"discarding:", "uncommitted files: 1", "unmerged commits: 1 on worktree-feature"} {
		if !strings.Contains(out+errb, want) {
			t.Errorf("summary %q lacks %q", out+errb, want)
		}
	}
}

func TestCmdWorktreeCleanForceNeverTouchesMainCheckout(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	for _, name := range []string{"..", "../" + filepath.Base(repo), repo, "."} {
		code, _, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"clean", "--force", name}) })
		if code == 0 {
			t.Errorf("clean --force %q must fail", name)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("main checkout damaged: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("unrelated branch lost")
	}
}

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
	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"create", "feature"}) })
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

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "--dry-run", "feature"}) })
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
	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "--no-verify", "feature"}) })
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

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "--no-verify", "--no-clean", "feature"}) })
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

	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--verify", "exit 1", "feature"})
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
	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--verify", verifyCmd, "feature"})
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

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "feature"}) })
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
	code, _, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "does-not-exist"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit for an unknown worktree name, stderr=%q", errb)
	}
}

func TestCmdWorktreeMergeTargetMismatchFails(t *testing.T) {
	mergeFixtureCLISetup(t)
	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--target", "some-other-branch", "feature"})
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
	code, _, _ := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge"}) })
	if code == 0 {
		t.Fatal("want non-zero exit for merge with no name")
	}
}

// TestCmdWorktreeMergeResolveAgentUnknownValueRejected covers --resolve-agent flag validation before
// anything else runs — no Preflight, no git mutation, no agent invocation.
func TestCmdWorktreeMergeResolveAgentUnknownValueRejected(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--resolve-agent", "gpt5", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for an unknown --resolve-agent value, stderr=%q", errb)
	}
	if !strings.Contains(errb, "unknown") {
		t.Fatalf("stderr = %q, want it to report the unknown value", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a rejected flag value")
	}
}

// TestCmdWorktreeMergeNoDetectableGateRefusesWithoutNoVerify covers the default (no --verify, no
// --no-verify) with nothing to auto-detect in the worktree: `worktree merge` must refuse outright
// before integrating anything, leaving both main and the worktree untouched — --no-verify is the
// only way around it.
func TestCmdWorktreeMergeNoDetectableGateRefusesWithoutNoVerify(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "feature"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit with no detectable gate and no --verify/--no-verify, stdout=%q", out)
	}
	if !strings.Contains(errb, "no verification gate found") {
		t.Fatalf("stderr = %q, want the no-gate message", errb)
	}
	if strings.Contains(out, "integrated") || strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, must never integrate or promote when refused for lack of a gate", out)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite the no-gate refusal")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a no-gate refusal: %v", err)
	}
}

// TestCmdWorktreeMergeNoVerifyPromotesWithoutRunningAnythingAndMarksUnverified covers --no-verify:
// the only way to promote with no gate at all, and its output must mark the merge as unverified
// rather than silently looking the same as a passing gate.
func TestCmdWorktreeMergeNoVerifyPromotesWithoutRunningAnythingAndMarksUnverified(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceOID := cliHeadOf(t, wt)

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "--no-verify", "feature"}) })
	if code != 0 {
		t.Fatalf("merge --no-verify: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("output = %q, want it to mark the merge as unverified", out)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
}

// TestCmdWorktreeMergeAutoDetectedFailingGateRefusesAndPreservesWorktree covers detection picking up
// a project gate (a Makefile `test:` target, inside the worktree) that fails: promotion must never
// run, and the worktree is left on disk for inspection, exactly like an explicit --verify failure.
func TestCmdWorktreeMergeAutoDetectedFailingGateRefusesAndPreservesWorktree(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "Makefile", "test:\n\texit 1\n", "add failing gate")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "feature"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit when the auto-detected gate fails, stdout=%q", out)
	}
	if strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, must never promote after a failing auto-detected gate", out)
	}
	if !strings.Contains(errb, "verify failed") {
		t.Fatalf("stderr = %q, want the verify-failed message", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite the failing auto-detected gate")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a failing auto-detected gate: %v", err)
	}
}

// TestCmdWorktreeMergeAutoDetectedPassingGatePromotes covers the success half: a Makefile `test:`
// target that passes is auto-detected and run, and promotion proceeds, naming the Makefile as source.
func TestCmdWorktreeMergeAutoDetectedPassingGatePromotes(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "Makefile", "test:\n\ttrue\n", "add passing gate")
	sourceOID := cliHeadOf(t, wt)

	code, out, errb := hosttest.CaptureOutput(t, func() int { return Run([]string{"merge", "feature"}) })
	if code != 0 {
		t.Fatalf("merge with a passing auto-detected gate: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "verify: passed (Makefile)") {
		t.Fatalf("output = %q, want a verify-passed line naming the Makefile", out)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
}

// TestCmdWorktreeMergeExplicitVerifyOverridesDetection covers --verify winning over a Makefile gate
// that would otherwise be auto-detected (and would fail): the explicit command is the only one run.
func TestCmdWorktreeMergeExplicitVerifyOverridesDetection(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "Makefile", "test:\n\texit 1\n", "add a failing gate that must be skipped")
	sourceOID := cliHeadOf(t, wt)

	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--verify", "true", "feature"})
	})
	if code != 0 {
		t.Fatalf("merge --verify overriding detection: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "verify: passed (flag)") {
		t.Fatalf("output = %q, want a verify-passed line naming the flag as the source", out)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
}

// TestCmdWorktreeMergeVerifyPipelineFailureFailsGate is the regression test for worktree.RunVerify's
// `set -o pipefail`: a pipeline whose first stage fails (`false | cat`) must fail the gate — without
// pipefail, the pipeline's exit status would be `cat`'s (0), silently reading as a passing gate.
func TestCmdWorktreeMergeVerifyPipelineFailureFailsGate(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"merge", "--verify", "false | cat", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for a failing pipeline stage under pipefail, stdout=%q", out)
	}
	if strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, must never promote after a failed pipeline verify", out)
	}
	if !strings.Contains(errb, "verify failed") {
		t.Fatalf("stderr = %q, want the verify-failed message", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite the failed pipeline verify")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a failed pipeline verify: %v", err)
	}
}

// TestWorktreeListJSONWriteError: a failed JSON write on stdout must exit non-zero, not succeed with partial output.
func TestWorktreeListJSONWriteError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(gitRepoCLI(t))

	code, _, _ := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"create", "test-wt"})
	})
	if code != 0 {
		t.Fatalf("create worktree failed: code=%d", code)
	}

	hosttest.ClosedStdout(t)
	code = Run([]string{"list", "--format", "json"})
	if code == 0 {
		t.Errorf("worktree list --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// TestWorktreeListEmptyJSONIsArray: an empty worktree list prints [] rather than null so consumers can iterate.
func TestWorktreeListEmptyJSONIsArray(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(gitRepoCLI(t))

	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"list", "--format", "json"})
	})
	if code != 0 {
		t.Fatalf("list empty: code=%d", code)
	}
	if strings.TrimSpace(out) == "null" {
		t.Errorf("empty list produced null, want []")
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Errorf("invalid JSON: %v, output=%q", err, out)
	}
	if entries == nil {
		t.Errorf("JSON unmarshaled to nil, want empty slice []")
	}
}
