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

	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/plans"
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
	t.Chdir(t.TempDir())
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

// TestCmdWorktreeGateOutsideRepo runs the gate verb outside git: a go.mod prints the full gate, a bare dir exits 1.
func TestCmdWorktreeGateOutsideRepo(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

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
	create := func(t *testing.T) string {
		t.Setenv("HOME", t.TempDir())
		t.Chdir(gitRepoCLI(t))
		_, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"create", "mergeable"}) })
		return strings.TrimSpace(out)
	}
	shared := create(t) // previews never remove it, so they share one fixture; the removing case builds its own
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
			dest := shared
			if tc.removed {
				dest = create(t)
			}
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

// Regression (codex-review #7): a worktree named with $(...) must never execute from the printed reopen command.
func TestCmdWorktreeListReopenCommandIsShellSafe(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
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
	repo = landFixtureCLISetup(t)
	wt = landFixtureCLIWorktreePath(t, repo)
	landFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
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

// A plain clean refuses a dirty (even merged) worktree and keeps it and its branch.
func TestCmdWorktreeCleanForceRefusedWithoutFlag(t *testing.T) {
	repo, wt := forceFixtureDirty(t)
	landFixtureCLIRun(t, repo, "merge", "-q", "--ff-only", "worktree-feature")
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
	repo := landFixtureCLISetup(t)
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

// landFixtureCLIWrite writes and commits a file, with the test identity set in local repo config.
func landFixtureCLIWrite(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	landFixtureCLIRun(t, dir, "add", file)
	landFixtureCLIRun(t, dir, "commit", "-q", "-m", msg)
}

func landFixtureCLIRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// landFixtureCLISetup makes a repo plus a "feature" worktree via the CLI create path, with a local git identity.
func landFixtureCLISetup(t *testing.T) (repo string) {
	t.Helper()
	t.Cleanup(worktree.SwapAuditCommand(""))
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

// landFixtureCLIWorktreePath resolves the feature worktree via worktree.Root, not a hand-built path.
func landFixtureCLIWorktreePath(t *testing.T, repo string) string {
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

// worktreeLandCase is one `worktree land` run on the "feature" fixture; {wt} in args is the worktree path.
type worktreeLandCase struct {
	name     string
	commit   [2]string // file, content committed in the worktree first; empty = none
	conflict bool      // main also edits f.txt, so the plan predicts a conflict
	args     []string
	ok       bool
	exit     int      // exact exit code wanted when set; otherwise any nonzero for !ok
	out      []string // stdout must contain each
	notOut   []string // stdout must contain none
	stderr   string
	landed   bool // g.txt holds the lane's content uncommitted in main
	dirty    bool // main has its own untracked g.txt first; the land keeps both its lines and the lane's
	kept     bool // the worktree is still on disk afterwards
}

var featureEdit = [2]string{"g.txt", "feature only\n"}

var worktreeLandCases = []worktreeLandCase{
	{name: "dry-run shows the clean plan", commit: featureEdit, args: []string{"--dry-run"}, ok: true,
		out: []string{"clean"}, kept: true},
	{name: "the retired --merge flag is an unknown flag", commit: featureEdit, args: []string{"--no-verify", "--merge"},
		exit: 2, stderr: "merge", kept: true},
	{name: "no-verify lands uncommitted, removes the worktree, keeps the branch", commit: featureEdit,
		args: []string{"--no-verify"}, ok: true, out: []string{"landed feature: 1 files uncommitted", "branch worktree-feature kept"},
		landed: true},
	{name: "a dirty landed path keeps both sides with markers", commit: featureEdit, dirty: true,
		args: []string{"--no-verify"}, ok: true, out: []string{"conflict markers left in (both sides changed the same lines): g.txt"}},
	{name: "no-clean leaves the worktree, resolve-agent unused on a clean plan", commit: featureEdit,
		args: []string{"--no-verify", "--no-clean", "--resolve-agent", "claude"}, ok: true,
		out: []string{"landed feature"}, landed: true, kept: true},
	{name: "explicit verify overrides detection and runs inside the worktree", commit: [2]string{"Makefile", "test:\n\texit 1\n"},
		args: []string{"--verify", `test -f Makefile && [ "$(pwd -P)" = "{wt}" ]`}, ok: true,
		out: []string{"verify: passed (flag)", "landed feature"}},
	{name: "pipefail fails a failing first stage", commit: featureEdit, args: []string{"--verify", "false | cat"},
		notOut: []string{"landed"}, stderr: "verify failed", kept: true},
	{name: "no detectable gate refuses", commit: featureEdit, notOut: []string{"integrated", "landed"},
		stderr: "no verification gate found", kept: true},
	{name: "unknown resolve-agent value", args: []string{"--resolve-agent", "gpt5"}, stderr: "unknown", kept: true},
	{name: "target mismatch", args: []string{"--target", "some-other-branch"}, out: []string{"target-mismatch"}, kept: true},
	{name: "dry-run resume needs an operation", args: []string{"--dry-run", "--resume"}, exit: 1,
		notOut: []string{"clean"}, stderr: "no in-progress --resolve-agent operation", kept: true},
}

func TestCmdWorktreeLand(t *testing.T) {
	for _, tc := range worktreeLandCases {
		t.Run(tc.name, func(t *testing.T) { runWorktreeLandCase(t, tc) })
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Chdir(gitRepoCLI(t)) // a bare repo suffices: neither arg set names an existing worktree
	for _, args := range [][]string{{"land"}, {"land", "does-not-exist"}} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree(args) }); code == 0 {
			t.Errorf("worktree %q: exit 0, want a failure", args)
		}
	}
}

func runWorktreeLandCase(t *testing.T, tc worktreeLandCase) {
	repo := landFixtureCLISetup(t)
	wt := landFixtureCLIWorktreePath(t, repo)
	if tc.commit[0] != "" {
		landFixtureCLIWrite(t, wt, tc.commit[0], tc.commit[1], "feature advances")
	}
	if tc.conflict {
		landFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")
	}
	mainBefore := cliHeadOf(t, repo)
	if tc.dirty {
		if err := os.WriteFile(filepath.Join(repo, "g.txt"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"land"}
	for _, a := range tc.args {
		args = append(args, strings.ReplaceAll(a, "{wt}", wt))
	}
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree(append(args, "feature")) })
	if (code == 0) != tc.ok || (tc.exit != 0 && code != tc.exit) || !strings.Contains(errb, tc.stderr) {
		t.Fatalf("worktree %q: code=%d stdout=%q stderr=%q, want ok=%v naming %q", args, code, out, errb, tc.ok, tc.stderr)
	}
	requireStdout(t, out, tc.out, tc.notOut)
	requireMainState(t, repo, wt, tc, mainBefore)
}

// requireMainState checks main's HEAD, its g.txt (landed or the owner's own) and whether the worktree remains.
func requireMainState(t *testing.T, repo, wt string, tc worktreeLandCase, wantHead string) {
	t.Helper()
	if cliHeadOf(t, repo) != wantHead {
		t.Errorf("main HEAD = %s, want unmoved %s", cliHeadOf(t, repo), wantHead)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "g.txt"))
	if tc.landed && string(got) != "feature only\n" || tc.dirty && (!strings.Contains(string(got), "mine\n") || !strings.Contains(string(got), "feature only\n")) {
		t.Errorf("main g.txt = %q (landed=%v dirty=%v)", got, tc.landed, tc.dirty)
	}
	if _, err := os.Stat(wt); (err == nil) != tc.kept {
		t.Errorf("worktree on disk = %v, want %v", err == nil, tc.kept)
	}
}

func requireStdout(t *testing.T, out string, wants, unwanteds []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	}
	for _, unwanted := range unwanteds {
		if strings.Contains(out, unwanted) {
			t.Errorf("stdout = %q, must not contain %q", out, unwanted)
		}
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
func TestCmdWorktreeLandResumeWrongRepoOperationFails(t *testing.T) {
	repo := landFixtureCLISetup(t)
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
		return cmdWorktree([]string{"land", "--resume", "feature"})
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
func TestCmdWorktreeLandResumeStopsOnTargetMovedSinceResolutionStarted(t *testing.T) {
	repo := landFixtureCLISetup(t)
	plan := resolvedLandOperationFixture(t, repo, "op-target-moved")
	mainBeforeResume := plan.TargetOID

	// Main advances on the same branch while the resolution is in flight.
	landFixtureCLIWrite(t, repo, "unrelated.txt", "main moved on\n", "main advances during resolution")
	movedMain := cliHeadOf(t, repo)
	if movedMain == mainBeforeResume {
		t.Fatal("fixture invariant broken: main must have actually moved")
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"land", "--resume", "feature"})
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
func TestCmdWorktreeLandResumeStopsOnTargetBranchChangedAtSameOID(t *testing.T) {
	repo := landFixtureCLISetup(t)
	plan := resolvedLandOperationFixture(t, repo, "op-branch-changed")

	// Switch to a new branch without moving the OID.
	landFixtureCLIRun(t, repo, "checkout", "-b", "renamed-main")
	sameOID := cliHeadOf(t, repo)
	if sameOID != plan.TargetOID {
		t.Fatal("fixture invariant broken: the checkout-b must not move the OID")
	}

	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"land", "--resume", "feature"})
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

func resolvedLandOperationFixture(t *testing.T, repo, id string) *worktree.LandPlan {
	t.Helper()
	wt := landFixtureCLIWorktreePath(t, repo)
	// Divergent edits predict exactly the conflict the simulated agent resolves below.
	landFixtureCLIWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	landFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")
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
	landFixtureCLIWrite(t, wt, "f.txt", "resolved\n", "agent resolves f")
	landFixtureCLIRun(t, wt, "merge", "--no-edit", "-s", "ours", plan.TargetOID)
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
func TestCmdWorktreeLandResumeMissingWorktreeFails(t *testing.T) {
	repo := landFixtureCLISetup(t)
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
		return cmdWorktree([]string{"land", "--resume", "ghost"})
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
	dest := landFixtureCLIWorktreePath(t, repo)
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

// worktree commit --dry-run lists the set and changes nothing (exit 0); a missing message is a usage error (2).
func TestCmdWorktreeCommitDryRunAndUsage(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	hosttest.WriteRel(t, repo, "a.txt", "one\ntwo\n")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.In(t, repo, "commit", "-qm", "chore: base")
	hosttest.WriteRel(t, repo, "a.txt", "one\ntwo\nthree\n")
	t.Chdir(repo)
	head := hosttest.In(t, repo, "rev-parse", "HEAD")
	code, out, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"commit", "-m", "fix: a", "a.txt", "--dry-run"})
	})
	if code != 0 || !strings.Contains(out, "a.txt  +1 −0") || hosttest.In(t, repo, "rev-parse", "HEAD") != head {
		t.Fatalf("dry run: exit %d, out %q, stderr %q", code, out, errb)
	}
	code, _, errb = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"commit", "a.txt"}) })
	if code != 2 || !strings.Contains(errb, "-m") {
		t.Fatalf("no message: exit %d, stderr %q; want 2 naming -m", code, errb)
	}
}

// worktree commit --list prints @id<TAB>path per dirty file without -m and commits nothing.
func TestCmdWorktreeCommitList(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	t.Chdir(repo)
	code, out, _ := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"commit", "--list"}) })
	if code != 0 || out != "" {
		t.Fatalf("clean: exit %d, out %q", code, out)
	}
	hosttest.WriteRel(t, repo, "a.txt", "one\n")
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"commit", "--list"}) })
	if f := strings.Split(strings.TrimSpace(out), "\t"); code != 0 || len(f) != 2 || f[1] != "a.txt" || !strings.HasPrefix(f[0], "@") {
		t.Fatalf("list: exit %d, out %q", code, out)
	}
}

// --plan outside the plans home is a usage error (2) before anything is staged or committed.
func TestCmdWorktreeCommitPlanOutsidePlansHomeIsUsage(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	hosttest.WriteRel(t, repo, "a.txt", "one\n")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.In(t, repo, "commit", "-qm", "chore: base")
	hosttest.WriteRel(t, repo, "a.txt", "one\ntwo\n")
	hosttest.WriteRel(t, repo, "2026-11-18-outside.md", "# x\n")
	t.Chdir(repo)
	head := hosttest.In(t, repo, "rev-parse", "HEAD")
	code, _, errb := hosttest.CaptureOutput(t, func() int {
		return cmdWorktree([]string{"commit", "-m", "fix: a", "--task", "x", "--plan", "2026-11-18-outside.md", "a.txt"})
	})
	if code != 2 || !strings.Contains(errb, "not a dated plan") || hosttest.In(t, repo, "rev-parse", "HEAD") != head {
		t.Fatalf("outside plan: exit %d, stderr %q; want 2 and no commit", code, errb)
	}
}

func TestCmdWorktreeGateRunPrintsObservedJSON(t *testing.T) {
	scrubGitEnv(t)
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goDir, "a.go"), []byte("package m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"gate", "--run", "--dir", goDir}) })
	if code != 0 || !strings.Contains(out, `"exit": 0`) || !strings.Contains(out, `"failures": []`) {
		t.Fatalf("gate --run: code=%d out=%q stderr=%q", code, out, errb)
	}
}

// A verified lane contract with its plan task ticked records tick and landed, then closes with its hand-back.
func TestRecordCloseTerms(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gitRepoCLI(t)
	hosttest.WriteRel(t, repo, "a.txt", "one\n")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.In(t, repo, "commit", "-qm", "chore: base")
	head := hosttest.In(t, repo, "rev-parse", "HEAD")
	hosttest.WriteRel(t, plans.Home(repo), "2026-01-01-x.md", "## Tasks\n- [x] Phase 1.1 — do it (verify: true) ("+head[:7]+")\n")
	saveVerifiedLaneContract(t, plans.Home(repo)+"/2026-01-01-x.md")

	var warn strings.Builder
	recordCloseTerms(onLane("other", plans.Home(repo)), head, &warn)
	recordCloseTerms(forTasks("", plans.Home(repo), plans.Home(repo)+"/2026-01-02-y.md", []string{"Phase 1.1"}), "", &warn)
	if got, _ := contract.Load("d1"); got.State() != contract.StateVerified {
		t.Fatalf("another lane or plan changed the contract: %s", got.State())
	}
	recordCloseTerms(onLane("l1", plans.Home(repo)), head, &warn)
	got, err := contract.Load("d1")
	if err != nil || warn.Len() > 0 || got.State() != contract.StateClosed || len(got.Unmet()) != 0 {
		t.Fatalf("err %v, warn %q, state %s, unmet %v", err, warn.String(), got.State(), got.Unmet())
	}
	for _, r := range got.Records[len(got.Records)-2:] {
		if r.By != contract.ByGo || !strings.HasPrefix(head, r.Evidence) {
			t.Errorf("record %+v, want evidence from %s", r, head)
		}
	}
}

// saveVerifiedLaneContract stores contract d1 on lane l1 for plan's Phase 1.1, verified and holding its hand-back.
func saveVerifiedLaneContract(t *testing.T, plan string) {
	t.Helper()
	c, ps := contract.Open(contract.Scope{Unit: "func x", Kind: contract.KindFunction, Own: []string{"a.txt"},
		PlanTask: "Phase 1.1", Plan: plan, Lane: "l1"},
		contract.Party{Role: contract.RoleOwner, Agent: "pi"}, contract.Limits{Depth: 1, Tokens: 1000, Leaves: 1},
		contract.Facts{Lane: true, PlanTask: true})
	if len(ps) > 0 {
		t.Fatal(ps)
	}
	pass := func(terms ...contract.Term) {
		for _, term := range terms {
			c, _ = c.WithRecord(contract.TermRecord{Term: term, Verdict: contract.VerdictPass, By: contract.ByGo})
		}
	}
	pass(contract.TermLane)
	c, _ = c.Start("d1")
	pass(contract.TermGate, contract.TermTests, contract.TermHandback, contract.TermReport)
	c, err := c.Verify(nil)
	c.Handback = &contract.Result{Status: contract.StatusDone, Tests: contract.TestsExecuted, Method: contract.MethodSolo}
	if err != nil || contract.Save(c) != nil {
		t.Fatal(err)
	}
}

// --drain --dry-run prints the plan and changes nothing: the lane stays queued, on disk and out of the live tree.
func TestCmdWorktreeLandDrainDryRunChangesNothing(t *testing.T) {
	repo := landFixtureCLISetup(t)
	wt := landFixtureCLIWorktreePath(t, repo)
	landFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	if err := worktree.Enqueue(worktree.QueuedLane{Lane: wt, Repo: repo, Reason: worktree.QueueReady, Next: "land"}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := hosttest.CaptureOutput(t, func() int { return cmdWorktree([]string{"land", "--drain", "--dry-run", "--no-verify"}) })
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errb)
	}
	requireStdout(t, out, []string{"dry run", "feature", "would land feature"}, []string{"landed"})
	if _, err := os.Stat(filepath.Join(repo, "g.txt")); err == nil {
		t.Error("dry-run drain landed the lane into the live tree")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("dry-run drain removed the worktree: %v", err)
	}
	if q := worktree.Queued(repo); len(q) != 1 {
		t.Errorf("queue = %+v, want the lane still queued", q)
	}
}
