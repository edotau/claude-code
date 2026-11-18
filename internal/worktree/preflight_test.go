package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths"
)

// mergeFixtureWrite writes content to file in dir and commits it with msg to build divergent history.
func mergeFixtureWrite(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureRun(t, dir, "add", file)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

// mergeFixtureRename renames oldName to newName and commits.
func mergeFixtureRename(t *testing.T, dir, oldName, newName, msg string) {
	t.Helper()
	mergeFixtureRun(t, dir, "mv", oldName, newName)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

// mergeFixtureRemove deletes file and commits the deletion.
func mergeFixtureRemove(t *testing.T, dir, file, msg string) {
	t.Helper()
	mergeFixtureRun(t, dir, "rm", "-q", file)
	mergeFixtureRun(t, dir, "commit", "-q", "-m", msg)
}

func mergeFixtureRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

var fixtureSeq atomic.Int64

var fixture struct {
	once sync.Once
	dir  string
	base string
	err  error
}

// buildFixtureTemplate builds, once, a main repo with the base commit and its "feature" worktree via the live Create.
func buildFixtureTemplate(t *testing.T) {
	fixture.once.Do(func() {
		dir, err := os.MkdirTemp("", "wt-fixture-")
		if err != nil {
			fixture.err = err
			return
		}
		fixture.dir = paths.Canon(dir)
		main := hosttest.RepoAt(t, filepath.Join(fixture.dir, "repo"))
		mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
		fixture.base = headOf(main)
		_, fixture.err = Create(&Repo{Main: main, Root: filepath.Join(fixture.dir, "worktrees", "repo")}, "feature", CreateOptions{})
	})
	if fixture.err != nil {
		t.Fatalf("fixture template: %v", fixture.err)
	}
}

func fixtureCopy(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput(); err != nil {
		t.Fatalf("copy %s to %s: %v %s", src, dst, err, out)
	}
}

// preflightFixtureSetup returns an isolated main repo plus a "feature" worktree, copied from a template built once.
func preflightFixtureSetup(t *testing.T) (repo *Repo, main, wt string) {
	t.Helper()
	hosttest.SkipOnWindows(t, "cp -a template copy")
	buildFixtureTemplate(t)
	base := paths.Canon(t.TempDir())
	name := fmt.Sprintf("repo%d", fixtureSeq.Add(1)) // unique: the ledger file name derives from the main's base name
	main = filepath.Join(base, name)
	repo = &Repo{Main: main, Root: filepath.Join(base, "worktrees", name)}
	wt = filepath.Join(repo.Root, "feature")
	fixtureCopy(t, filepath.Join(fixture.dir, "repo"), main)
	fixtureCopy(t, filepath.Join(fixture.dir, "worktrees", "repo", "feature"), wt)
	// the two absolute links a copy carries point at the template; rewrite them to this copy
	adminDir := filepath.Join(main, ".git", "worktrees", "feature")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+adminDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(wt, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := ledgerInfo{Path: wt, Branch: "worktree-feature", Base: fixture.base, BaseRef: "HEAD"}
	if err := openLedger(main, "feature", info); err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	return repo, main, wt
}

// mergeFixtureAssertMainUntouched checks main's HEAD and index are unchanged and no MERGE_HEAD exists there.
func mergeFixtureAssertMainUntouched(t *testing.T, main, wantHEAD string) {
	t.Helper()
	if got := headOf(main); got != wantHEAD {
		t.Fatalf("main HEAD = %s, want %s (main must stay untouched before promotion)", got, wantHEAD)
	}
	stCmd := exec.Command("git", "-C", main, "status", "--porcelain")
	stCmd.Env = gitEnv()
	out, err := stCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("main checkout has uncommitted changes: %q", out)
	}
	gitDir, _, err := gitDirsOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		t.Fatal("MERGE_HEAD exists in the main checkout after a predicted conflict")
	}
}

func TestScreenCheckoutsStopsOnTargetMismatch(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	plan := &MergePlan{WorktreePath: filepath.Join(t.TempDir(), "missing-worktree")}
	stop, err := screenCheckouts(context.Background(), repo, plan, "release", false)
	if err != nil || !stop {
		t.Fatalf("target mismatch must stop before inspecting the nonexistent worktree: stop=%v err=%v", stop, err)
	}
	if plan.TargetRef != "main" || len(plan.Issues) != 1 {
		t.Fatalf("branch screen did not preserve target and issue: %+v", plan)
	}
	issue := plan.Issues[0]
	if issue.Code != issueTargetMismatch || !strings.Contains(issue.Detail, `requested target was "release"`) {
		t.Fatalf("issue = %+v, want requested-target mismatch", issue)
	}
	if plan.TargetOID != "" || plan.SourceOID != "" {
		t.Fatalf("mismatched target must not pin OIDs: %+v", plan)
	}
}

func TestPreflightFastForward(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if plan.TargetRef != "main" {
		t.Fatalf("TargetRef = %q, want main", plan.TargetRef)
	}
	if plan.TargetOID != headOf(main) || plan.SourceOID != headOf(wt) {
		t.Fatalf("OIDs not pinned to live HEADs: plan=%+v", plan)
	}
}

func TestPreflightDivergentCleanMerge(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	mergeFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none (non-overlapping changes)", plan.Issues)
	}
}

func TestPreflightTextConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main line\n", "main edits f")
	mainHEAD := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
	mergeFixtureAssertMainUntouched(t, main, mainHEAD)
}

func TestPreflightBinaryConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "\x00\x01binA", "feature binary")
	mergeFixtureWrite(t, main, "f.txt", "\x00\x01binB", "main binary")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightDeleteConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature modify\n", "feature modifies f")
	mergeFixtureRemove(t, main, "f.txt", "main deletes f")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if !slices.Contains(plan.Issues[0].Paths, "f.txt") {
		t.Fatalf("conflict paths = %v, want f.txt", plan.Issues[0].Paths)
	}
}

func TestPreflightRenameConflict(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	// Rename/rename to two different names conflicts in merge-tree, unlike a rename plus a non-conflicting edit.
	mergeFixtureRename(t, wt, "f.txt", "g.txt", "feature renames to g")
	mergeFixtureRename(t, main, "f.txt", "h.txt", "main renames to h")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want one IssueConflict", plan.Issues)
	}
	if len(plan.Issues[0].Paths) == 0 {
		t.Fatal("rename/rename conflict reported no paths")
	}
}

func assertDirtyTargetIssue(t *testing.T, plan *MergePlan, path string) {
	t.Helper()
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyTarget {
		t.Fatalf("Issues = %v, want one IssueDirtyTarget", plan.Issues)
	}
	issue := plan.Issues[0]
	if !slices.Equal(issue.Paths, []string{path}) || !strings.Contains(issue.Detail, path) {
		t.Fatalf("issue = %+v, want only overlapping path %q and detail naming it", issue, path)
	}
}

func TestPreflightDirtyTargetNonOverlapping(t *testing.T) {
	for _, name := range []string{"f.txt", "dirty\nfile.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, main, wt := preflightFixtureSetup(t)
			mergeFixtureWrite(t, main, "f.txt", "main edit\n", "main edits f")
			mergeFixtureWrite(t, wt, "g.txt", "feature\n", "feature adds g")
			if err := os.WriteFile(filepath.Join(main, name), []byte("uncommitted"), 0o644); err != nil {
				t.Fatal(err)
			}
			plan, err := Preflight(context.Background(), repo, "feature", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Issues) != 0 || plan.TargetOID != headOf(main) || plan.SourceOID != headOf(wt) {
				t.Fatalf("non-overlapping dirty target blocked or OIDs not pinned: %+v", plan)
			}
		})
	}
}

func TestPreflightDirtyTargetRename(t *testing.T) {
	for _, name := range []string{"f.txt", "renamed\nfile.txt"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo, main, wt := preflightFixtureSetup(t)
			mergeFixtureWrite(t, wt, name, "feature\n", "feature edits path")
			mergeFixtureRun(t, main, "mv", "f.txt", "renamed\nfile.txt")
			plan, err := Preflight(context.Background(), repo, "feature", "")
			if err != nil {
				t.Fatal(err)
			}
			assertDirtyTargetIssue(t, plan, name)
		})
	}
}

func TestPreflightPromotedRenameOverlapsOldPath(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureRename(t, wt, "f.txt", "renamed.txt", "feature renames f")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	assertDirtyTargetIssue(t, plan, "f.txt")
}

// Regression: status.showUntrackedFiles=no must not hide a dirty worktree from isDirty.
func TestPreflightDirtyWorktreeWithShowUntrackedFilesOff(t *testing.T) {
	t.Parallel()
	repo, _, wt := preflightFixtureSetup(t)
	mergeFixtureRun(t, wt, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(wt, "untracked.txt"), []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree (an untracked file must still count as dirty)", plan.Issues)
	}
}

func TestPreflightDirtyWorktree(t *testing.T) {
	t.Parallel()
	repo, _, wt := preflightFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDirtyWorktree {
		t.Fatalf("Issues = %v, want one IssueDirtyWorktree", plan.Issues)
	}
}

func TestPreflightDetachedTarget(t *testing.T) {
	t.Parallel()
	repo, main, _ := preflightFixtureSetup(t)
	head := headOf(main)
	mergeFixtureRun(t, main, "checkout", "-q", head)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueDetachedTarget {
		t.Fatalf("Issues = %v, want one IssueDetachedTarget", plan.Issues)
	}
}

func TestPreflightTargetMismatch(t *testing.T) {
	t.Parallel()
	repo, _, _ := preflightFixtureSetup(t)

	plan, err := Preflight(context.Background(), repo, "feature", "some-other-branch")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueTargetMismatch {
		t.Fatalf("Issues = %v, want one IssueTargetMismatch", plan.Issues)
	}
}

func TestPreflightInProgressOperation(t *testing.T) {
	t.Parallel()
	repo, main, _ := preflightFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(main, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir, _, err := gitDirsOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(headOf(main)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueInProgress {
		t.Fatalf("Issues = %v, want one IssueInProgress", plan.Issues)
	}
}

func TestPreflightUnrelatedHistories(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureRun(t, wt, "checkout", "-q", "--orphan", "orphan-base")
	mergeFixtureRun(t, wt, "commit", "-q", "--allow-empty", "-m", "orphan root")
	_ = main

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueUnrelatedHistories {
		t.Fatalf("Issues = %v, want one IssueUnrelatedHistories", plan.Issues)
	}
}

// Regression: a non-NotExist stat failure (permission denied) must propagate, never read as no operation in progress.
func TestInProgressOpPropagatesNonNotExistStatError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("permission checks don't apply when running as root")
	}
	_, _, wt := preflightFixtureSetup(t)
	gitDir, _, err := gitDirsOf(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })

	if _, _, err := inProgressOp(context.Background(), wt, ""); err == nil {
		t.Fatal("inProgressOp: want a propagated error for a permission-denied stat, got nil")
	}
}

func TestPreflightMissingWorktree(t *testing.T) {
	t.Parallel()
	repo, _, _ := preflightFixtureSetup(t)
	if _, err := Preflight(context.Background(), repo, "does-not-exist", ""); err == nil {
		t.Fatal("want error for a nonexistent worktree")
	}
}

// A fake runGit reporting merge-tree's absence (git < 2.38, exit outside {0,1}) must block Preflight.
func TestSimulateMergeUnsupportedGit(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	repo, _, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	orig := runGit
	runGit = func(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "merge-tree" {
			return nil, &exec.ExitError{ProcessState: mergeFixtureFakeExitState(129)}
		}
		return orig(ctx, dir, extraEnv, args...)
	}
	defer func() { runGit = orig }()

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != issueCapability {
		t.Fatalf("Issues = %v, want one IssueCapability", plan.Issues)
	}
}

// mergeFixtureFakeExitState runs a trivial subprocess to get a real *os.ProcessState, which exec.ExitError requires.
func mergeFixtureFakeExitState(code int) *os.ProcessState {
	cmd := exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	_ = cmd.Run()
	return cmd.ProcessState
}

func TestPlanOrResolveConflictProceedsToRebase(t *testing.T) {
	repo := conflictedOwnedWorktree(t, "owner-S")
	req := laneRequest("feat", io.Discard)
	for _, dry := range []bool{true, false} {
		plan, err := PlanOrResolve(t.Context(), repo, req, dry)
		if err != nil || plan == nil || !onlyConflictIssue(plan.Issues) {
			t.Fatalf("plan = %+v, %v; want predicted conflict for rebase", plan, err)
		}
	}
	got, err := DrainErr("owner-S")
	if err != nil || len(got) != 0 {
		t.Fatalf("planning wrote notices: %+v %v", got, err)
	}
}

// conflictedOwnedWorktree is a repo whose worktree "feat", opened by session, conflicts with main on f.txt.
func conflictedOwnedWorktree(t *testing.T, session string) *Repo {
	t.Helper()
	home := paths.Canon(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	main := gitRepo(t)
	mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := Create(repo, "feat", CreateOptions{Session: session, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	mergeFixtureWrite(t, wt, "f.txt", "feature\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main\n", "main edits f")
	return repo
}

// TestPreflightDirtyTargetNamesClaimHolder: an overlapping dirty file names the live session that claims it.
func TestPreflightDirtyTargetNamesClaimHolder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature edit\n", "feature edits f")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	claims := filepath.Join(ClaimsDir(), fmt.Sprintf("%d-peer-session.jsonl", os.Getpid()))
	if err := os.MkdirAll(filepath.Dir(claims), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"schema":1,"path":%q,"ts":1}`+"\n", filepath.Join(main, "f.txt"))
	if err := os.WriteFile(claims, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	assertDirtyTargetIssue(t, plan, "f.txt")
	if plan.TargetOID != "" || plan.SourceOID != "" {
		t.Fatalf("OIDs pinned despite an overlapping dirty target: %+v", plan)
	}
	want := fmt.Sprintf("f.txt (held by session peer-ses (pid %d))", os.Getpid())
	if !strings.Contains(plan.Issues[0].Detail, want) {
		t.Fatalf("detail = %q, want %q", plan.Issues[0].Detail, want)
	}
}

// A live process other than the caller inside the lane refuses the merge; the caller's own process does not.
func TestPreflightRefusesLaneInUseByAnotherProcess(t *testing.T) {
	repo := conflictedOwnedWorktree(t, "owner-S")
	for _, tc := range []struct {
		pid  int
		busy bool
	}{{os.Getpid(), false}, {os.Getpid() + 100000, true}} {
		stubProcesses(t, func(string) ([]Proc, error) { return []Proc{{PID: tc.pid, Comm: "vim"}}, nil })
		plan, err := Preflight(t.Context(), repo, "feat", "")
		if err != nil {
			t.Fatal(err)
		}
		inUse := slices.ContainsFunc(plan.Issues, func(iss Issue) bool { return strings.Contains(iss.Detail, "lane in use by pid") })
		if inUse != tc.busy {
			t.Fatalf("pid %d: issues %+v, want lane-in-use %v", tc.pid, plan.Issues, tc.busy)
		}
	}
}
