package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// Bare worktree lists (exit 0); an unknown verb, a wrong arity or an unknown name fails.
func TestCmdWorktreeNoArgsAndUnknownSub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(gitRepoCLI(t))
	for _, tc := range []struct {
		args []string
		want int
	}{{nil, 0}, {[]string{"bogus"}, 2}, {[]string{"create"}, 1}, {[]string{"create", "a", "b"}, 1},
		{[]string{"clean", "does-not-exist"}, 1}} {
		code, _, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree(tc.args) })
		if (code == 0) != (tc.want == 0) || (tc.want == 2 && code != 2) {
			t.Errorf("worktree %q: exit %d, want %d; stderr=%q", tc.args, code, tc.want, errb)
		}
	}
}

func TestCmdWorktreeNonRepoCWD(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir()) // not a git repository
	code, _, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
	if code == 0 {
		t.Fatalf("want non-zero exit outside a git repo, stderr=%q", errb)
	}
}

func TestCmdWorktreeListEmptyTextAndJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)

	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "no worktrees under") {
		t.Fatalf("text list empty: code=%d out=%q", code, out)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"list", "--format", "json"}) })
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

// TestCmdWorktreeGateOutsideRepo runs the gate verb in a non-git dir: a go.mod prints the full gate, a bare dir exits 1.
func TestCmdWorktreeGateOutsideRepo(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // not a git repository

	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"gate", "--dir", goDir}) })
	if code != 0 || strings.TrimSpace(out) != "go build ./... && go vet ./... && go test ./..." {
		t.Fatalf("gate on go.mod: code=%d out=%q stderr=%q", code, out, errb)
	}
	code, _, errb = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"gate", "--dir", t.TempDir()}) })
	if code != 1 || !strings.Contains(errb, "no verification gate found") {
		t.Fatalf("gate with no toolchain: code=%d stderr=%q, want 1 and the no-gate message", code, errb)
	}
	code, _, _ = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"gate", "-h"}) })
	if code != 0 {
		t.Fatalf("gate -h: code=%d, want 0", code)
	}
}

func TestCmdWorktreeGateWriteFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	closed, err := os.CreateTemp(t.TempDir(), "closed-stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				stdout := os.Stdout
				os.Stdout = closed
				defer func() { os.Stdout = stdout }()
				return cmdWorktreeGate([]string{"--dir", dir, "--format", format})
			})
			if code != 1 || !strings.Contains(stderr, "worktree gate:") || !strings.Contains(stderr, closed.Name()) {
				t.Fatalf("write to closed stdout: code=%d stderr=%q, want exit 1 and write error", code, stderr)
			}
		})
	}
}

// create prints the new worktree's path on its branch (default worktree-<name>) and list then shows it.
func TestCmdWorktreeCreateThenListShowsIt(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		branch string
	}{
		{[]string{"create", "feature"}, "worktree-feature"},
		{[]string{"create", "--branch", "wt/custom", "feature"}, "wt/custom"},
		{[]string{"create", "dots.and-dashes_123"}, "worktree-dots.and-dashes_123"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(gitRepoCLI(t))
			code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree(tc.args) })
			if code != 0 {
				t.Fatalf("create: code=%d stderr=%q", code, errb)
			}
			branch, err := exec.Command("git", "-C", strings.TrimSpace(out), "branch", "--show-current").Output()
			if err != nil || strings.TrimSpace(string(branch)) != tc.branch {
				t.Fatalf("branch = %q, err %v, want %s", branch, err, tc.branch)
			}
			_, out, _ = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"list", "--format", "json"}) })
			var entries []map[string]any
			if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) != 1 {
				t.Fatalf("list = %q (err %v), want one entry", out, err)
			}
		})
	}
}

// Only a plain clean of an eligible worktree removes it; --dry-run and clean-all without --yes only preview.
func TestCmdWorktreeCleanEligible(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    string
		removed bool
	}{
		{[]string{"clean", "--dry-run", "mergeable"}, "eligible:", false},
		{[]string{"clean", "mergeable"}, "removed:", true},
		{[]string{"clean-all"}, "preview only", false},
		{[]string{"clean-all", "--dry-run", "--yes"}, "preview only", false}, // the safer flag wins
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(gitRepoCLI(t))
			_, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
			dest := strings.TrimSpace(out)
			code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree(tc.args) })
			if code != 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("code=%d out=%q, want 0 and %q", code, out, tc.want)
			}
			if _, err := os.Stat(dest); os.IsNotExist(err) != tc.removed {
				t.Fatalf("worktree removed = %v, want %v", os.IsNotExist(err), tc.removed)
			}
		})
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
		return cmdWorktree([]string{"create", "--branch", "wt/safe-branch", name})
	})
	if code != 0 {
		t.Fatalf("create: code=%d stderr=%q", code, errb)
	}
	dest := strings.TrimSpace(out)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("worktree missing at %s: %v", dest, err)
	}

	code, listOut, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"list"}) })
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

// A plain clean refuses a dirty worktree, merged or not, and keeps it and its branch.
func TestCmdWorktreeCleanForceRefusedWithoutFlag(t *testing.T) {
	for _, unmerged := range []bool{false, true} {
		t.Run(map[bool]string{false: "dirty", true: "dirty+unmerged"}[unmerged], func(t *testing.T) {
			repo, wt := forceFixtureDirty(t)
			if !unmerged {
				mergeFixtureCLIRun(t, repo, "merge", "-q", "--ff-only", "worktree-feature")
			}
			code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"clean", "feature"}) })
			if code == 0 || !strings.Contains(out, "refused:") {
				t.Fatalf("plain clean of a dirty worktree: code=%d out=%q, want a refused: line", code, out)
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("worktree must survive a refused clean: %v", err)
			}
			if forceBranchList(t, repo, "worktree-feature") == "" {
				t.Fatal("branch must survive a refused clean")
			}
		})
	}
}

func TestCmdWorktreeCleanForceDiscardsDirtyUnmerged(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"clean", "--force", "feature"}) })
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
		code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"clean", "--force", name}) })
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
	t.Chdir(repo)
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"create", "feature"}) })
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

// worktreeMergeCase is one `worktree merge` run on the "feature" fixture; {wt} in args is the worktree path.
type worktreeMergeCase struct {
	name     string
	commit   [2]string // file, content committed in the worktree first; empty = none
	conflict bool      // main also edits f.txt, so the plan predicts a conflict
	args     []string
	ok       bool
	exit     int      // exact exit code wanted when set; otherwise any nonzero for !ok
	out      []string // stdout must contain each
	notOut   []string // stdout must contain none
	stderr   string
	promoted bool // main fast-forwards to the worktree's head; otherwise main never moves
	kept     bool // the worktree is still on disk afterwards
}

var featureEdit = [2]string{"g.txt", "feature only\n"}

var worktreeMergeCases = []worktreeMergeCase{
	{name: "dry-run shows the clean plan", commit: featureEdit, args: []string{"--dry-run"}, ok: true,
		out: []string{"clean"}, kept: true},
	{name: "no-verify promotes, auto-cleans, marks unverified", commit: featureEdit, args: []string{"--no-verify"},
		ok: true, out: []string{"promoted", "unverified"}, promoted: true},
	{name: "no-clean leaves the worktree", commit: featureEdit, args: []string{"--no-verify", "--no-clean"}, ok: true,
		out: []string{"promoted"}, promoted: true, kept: true},
	{name: "failing verify skips promotion", commit: featureEdit, args: []string{"--verify", "exit 1"},
		notOut: []string{"promoted"}, kept: true},
	{name: "passing verify runs inside the worktree", commit: featureEdit,
		args: []string{"--verify", `test -f g.txt && [ "$(pwd -P)" = "{wt}" ]`}, ok: true,
		out: []string{"verify: passed", "promoted"}, promoted: true},
	{name: "pipefail fails a failing first stage", commit: featureEdit, args: []string{"--verify", "false | cat"},
		notOut: []string{"promoted"}, stderr: "verify failed", kept: true},
	{name: "predicted conflict blocks", commit: [2]string{"f.txt", "feature line\n"}, conflict: true,
		out: []string{"conflict"}, kept: true},
	{name: "no detectable gate refuses", commit: featureEdit, notOut: []string{"integrated", "promoted"},
		stderr: "no verification gate found", kept: true},
	{name: "auto-detected failing gate", commit: [2]string{"Makefile", "test:\n\texit 1\n"},
		notOut: []string{"promoted"}, stderr: "verify failed", kept: true},
	{name: "auto-detected passing gate", commit: [2]string{"Makefile", "test:\n\ttrue\n"}, ok: true,
		out: []string{"verify: passed (Makefile)", "promoted"}, promoted: true},
	{name: "explicit verify overrides detection", commit: [2]string{"Makefile", "test:\n\texit 1\n"},
		args: []string{"--verify", "true"}, ok: true, out: []string{"verify: passed (flag)", "promoted"}, promoted: true},
	{name: "resolve-agent unused on a clean plan", commit: featureEdit,
		args: []string{"--no-verify", "--resolve-agent", "claude"}, ok: true, out: []string{"promoted"}, promoted: true},
	{name: "unknown resolve-agent value", args: []string{"--resolve-agent", "gpt5"}, stderr: "unknown", kept: true},
	{name: "target mismatch", args: []string{"--target", "some-other-branch"}, out: []string{"target-mismatch"}, kept: true},
	{name: "resume with no saved operation", args: []string{"--resume"}, stderr: "no in-progress", kept: true},
	{name: "dry-run resume needs an operation", args: []string{"--dry-run", "--resume"}, exit: 1,
		notOut: []string{"clean"}, stderr: "no in-progress --resolve-agent operation", kept: true},
}

func TestCmdWorktreeMerge(t *testing.T) {
	for _, tc := range worktreeMergeCases {
		t.Run(tc.name, func(t *testing.T) { runWorktreeMergeCase(t, tc) })
	}
	for _, args := range [][]string{{"merge"}, {"merge", "does-not-exist"}} {
		mergeFixtureCLISetup(t)
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree(args) }); code == 0 {
			t.Errorf("worktree %q: exit 0, want a failure", args)
		}
	}
}

func runWorktreeMergeCase(t *testing.T, tc worktreeMergeCase) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	if tc.commit[0] != "" {
		mergeFixtureCLIWrite(t, wt, tc.commit[0], tc.commit[1], "feature advances")
	}
	if tc.conflict {
		mergeFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")
	}
	mainBefore, source := cliHeadOf(t, repo), cliHeadOf(t, wt)
	args := []string{"merge"}
	for _, a := range tc.args {
		args = append(args, strings.ReplaceAll(a, "{wt}", wt))
	}
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree(append(args, "feature")) })
	if (code == 0) != tc.ok || (tc.exit != 0 && code != tc.exit) || !strings.Contains(errb, tc.stderr) {
		t.Fatalf("worktree %q: code=%d stdout=%q stderr=%q, want ok=%v naming %q", args, code, out, errb, tc.ok, tc.stderr)
	}
	for _, want := range tc.out {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	}
	for _, unwanted := range tc.notOut {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout = %q, must not contain %q", out, unwanted)
		}
	}
	if want := map[bool]string{false: mainBefore, true: source}[tc.promoted]; cliHeadOf(t, repo) != want {
		t.Errorf("main HEAD = %s, want %s (promoted=%v)", cliHeadOf(t, repo), want, tc.promoted)
	}
	if _, err := os.Stat(wt); (err == nil) != tc.kept {
		t.Errorf("worktree on disk = %v, want %v", err == nil, tc.kept)
	}
}

// TestWorktreeListJSONWriteError: a failed JSON write on stdout must exit non-zero, not succeed with partial output.
func TestWorktreeListJSONWriteError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(gitRepoCLI(t))

	code, _, _ := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"create", "test-wt"})
	})
	if code != 0 {
		t.Fatalf("create worktree failed: code=%d", code)
	}

	hosttest.ClosedStdout(t)
	code = cmdWorktree([]string{"list", "--format", "json"})
	if code == 0 {
		t.Errorf("worktree list --format json on closed stdout: exit %d, want non-zero", code)
	}
}

// An operation saved for another repo must not resume against this checkout.
func TestCmdWorktreeMergeResumeWrongRepoOperationFails(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	r, err := worktree.ResolveRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	// operationPath hashes the full path, so the same basename under another parent cannot match.
	otherRepoWithSameBasename := filepath.Join(r.Main, "..", "other-"+filepath.Base(r.Main), filepath.Base(r.Main))
	op := &worktree.Operation{
		Repo:      otherRepoWithSameBasename,
		Worktree:  "feature",
		Phase:     "resolving",
		Agent:     "claude",
		StartedAt: time.Now(),
		Deadline:  time.Now().Add(10 * time.Minute),
	}
	if err := worktree.SaveOperation(op); err != nil {
		t.Fatal(err)
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for a wrong-repo operation, stderr=%q", errb)
	}
	if !strings.Contains(errb, "no in-progress") {
		t.Fatalf("stderr = %q, want no operation found for this repo", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a wrong-repo operation")
	}
}

// Compare the pinned target OID, not only branch names, when resuming an in-flight resolution.
func TestCmdWorktreeMergeResumeStopsOnTargetMovedSinceResolutionStarted(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	plan := resolvedMergeOperationFixture(t, repo, "op-target-moved")
	mainBeforeResume := plan.TargetOID

	// Main advances on the same branch while the resolution is in flight.
	mergeFixtureCLIWrite(t, repo, "unrelated.txt", "main moved on\n", "main advances during resolution")
	movedMain := cliHeadOf(t, repo)
	if movedMain == mainBeforeResume {
		t.Fatal("fixture invariant broken: main must have actually moved")
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit when the target moved during resolution, stderr=%q", errb)
	}
	if !strings.Contains(errb, "moved") {
		t.Fatalf("stderr = %q, want it to report the target movement", errb)
	}
	if cliHeadOf(t, repo) != movedMain {
		t.Fatal("main HEAD changed despite target movement, which must stop before integrate/promote")
	}
}

// Compare TargetRef too: another branch at the same pinned OID is still target movement.
func TestCmdWorktreeMergeResumeStopsOnTargetBranchChangedAtSameOID(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	plan := resolvedMergeOperationFixture(t, repo, "op-branch-changed")

	// Switch to a new branch without moving the OID.
	mergeFixtureCLIRun(t, repo, "checkout", "-b", "renamed-main")
	sameOID := cliHeadOf(t, repo)
	if sameOID != plan.TargetOID {
		t.Fatal("fixture invariant broken: the checkout-b must not move the OID")
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit when the target branch changed at the same OID, stderr=%q", errb)
	}
	if !strings.Contains(errb, "moved") {
		t.Fatalf("stderr = %q, want it to report the target movement", errb)
	}
	if cliHeadOf(t, repo) != sameOID {
		t.Fatal("main HEAD changed despite the branch-change refusal")
	}
}

func resolvedMergeOperationFixture(t *testing.T, repo, id string) *worktree.MergePlan {
	t.Helper()
	wt := mergeFixtureCLIWorktreePath(t, repo)
	// Divergent edits predict exactly the conflict the simulated agent resolves below.
	mergeFixtureCLIWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")
	r, err := worktree.ResolveRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := worktree.Preflight(context.Background(), r, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != worktree.IssueConflict {
		t.Fatalf("Issues = %v, want exactly one conflict", plan.Issues)
	}
	// Record both pinned OIDs as ancestors, satisfying CheckResolution's contract.
	mergeFixtureCLIWrite(t, wt, "f.txt", "resolved\n", "agent resolves f")
	mergeFixtureCLIRun(t, wt, "merge", "--no-edit", "-s", "ours", plan.TargetOID)
	op := &worktree.Operation{
		ID:                id,
		Repo:              r.Main,
		Worktree:          "feature",
		TargetRef:         plan.TargetRef,
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		ConflictPaths:     []string{"f.txt"},
		Phase:             "resolving",
		StartedAt:         time.Now(),
		Deadline:          time.Now().Add(10 * time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := worktree.SaveOperation(op); err != nil {
		t.Fatal(err)
	}
	return plan
}

// A missing worktree must fail clearly before polling it or changing main.
func TestCmdWorktreeMergeResumeMissingWorktreeFails(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	r, err := worktree.ResolveRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	op := &worktree.Operation{
		Repo:      r.Main,
		Worktree:  "ghost",
		Phase:     "resolving",
		Agent:     "claude",
		StartedAt: time.Now(),
		Deadline:  time.Now().Add(10 * time.Minute),
	}
	if err := worktree.SaveOperation(op); err != nil {
		t.Fatal(err)
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "ghost"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for a missing worktree, stderr=%q", errb)
	}
	if !strings.Contains(errb, "not found") {
		t.Fatalf("stderr = %q, want a not-found message", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a missing worktree")
	}
}

func TestCmdWorktreeCleanForceBranchDeletionFailure(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	lock := filepath.Join(repo, ".git", "refs", "heads", "worktree-feature.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"clean", "--force", "feature"})
	})
	if code != 1 || !strings.Contains(stderr, "branch not deleted") || !strings.Contains(stderr, "worktree-feature.lock") {
		t.Fatalf("locked branch deletion: code=%d stdout=%q stderr=%q", code, out, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed despite branch deletion failure: %v", err)
	}
	if forceBranchList(t, repo, "worktree-feature") == "" {
		t.Fatal("locked branch should remain after failed deletion")
	}
}

func TestCmdWorktreeCreateStdoutFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	unwritable, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unwritable.Close(); err != nil {
			t.Error(err)
		}
	})
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		stdout := os.Stdout
		os.Stdout = unwritable
		defer func() { os.Stdout = stdout }()
		return cmdWorktree([]string{"create", "feature"})
	})
	dest := mergeFixtureCLIWorktreePath(t, repo)
	if code != 1 || !strings.Contains(stderr, "could not print its path") || !strings.Contains(stderr, dest) {
		t.Fatalf("create with unwritable stdout: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("created worktree should remain at reported path %s: %v", dest, err)
	}
}

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
	return hosttest.Repo(t)
}
