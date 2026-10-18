package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/worktree"
)

// TestCmdWorktreeMergeResolveAgentUnknownValueRejected covers --resolve-agent flag validation before
// anything else runs — no Preflight, no git mutation, no agent invocation.
func TestCmdWorktreeMergeResolveAgentUnknownValueRejected(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	code, _, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resolve-agent", "gpt5", "feature"})
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

// TestCmdWorktreeMergeResolveAgentIgnoredOnCleanPlan: --resolve-agent only ever applies to a predicted
// conflict (onlyConflictIssue) — a clean fast-forward must proceed and promote normally without ever
// reaching the agent-dispatch code path (there is no fake-agent seam across this package boundary, so
// this also doubles as proof the real `claude-code ask` is never invoked for a clean plan).
func TestCmdWorktreeMergeResolveAgentIgnoredOnCleanPlan(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceOID := cliHeadOf(t, wt)

	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resolve-agent", "claude", "feature"})
	})
	if code != 0 {
		t.Fatalf("merge --resolve-agent on a clean plan: code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "promoted") {
		t.Fatalf("output = %q, want a promoted line", out)
	}
	if cliHeadOf(t, repo) != sourceOID {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", cliHeadOf(t, repo), sourceOID)
	}
}

// TestCmdWorktreeMergeResumeWithNoSavedOperationFails covers --resume with nothing to resume: it must
// surface LoadOperation's not-found error and leave main untouched, never fabricate a resolution.
func TestCmdWorktreeMergeResumeWithNoSavedOperationFails(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	code, _, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for --resume with no saved operation, stderr=%q", errb)
	}
	if !strings.Contains(errb, "no in-progress") {
		t.Fatalf("stderr = %q, want the no-in-progress-operation message", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite nothing to resume")
	}
}

// TestCmdWorktreeMergeResumeWrongRepoOperationFails hand-crafts an Operation saved for a different
// repo.Main (e.g. left over from a previous repo at this worktree root) and confirms --resume refuses
// it rather than resuming against the wrong checkout.
func TestCmdWorktreeMergeResumeWrongRepoOperationFails(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	mainBefore := cliHeadOf(t, repo)
	r, err := worktree.ResolveRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	// Same basename as the real repo.Main (operationPath keys by it), different full path — so
	// LoadOperation finds this file, but resumeWorktreeMerge's op.Repo != repo.Main check must fire.
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

	code, _, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--resume", "feature"})
	})
	if code == 0 {
		t.Fatalf("want non-zero exit for a wrong-repo operation, stderr=%q", errb)
	}
	if !strings.Contains(errb, "belongs to") {
		t.Fatalf("stderr = %q, want the belongs-to-a-different-repo message", errb)
	}
	if cliHeadOf(t, repo) != mainBefore {
		t.Fatal("main HEAD changed despite a wrong-repo operation")
	}
}

// TestCmdWorktreeMergeResumeStopsOnTargetMovedSinceResolutionStarted is the regression test for
// Appendix A codex-review finding #6 (internal/cli/worktree.go:256, resume half): if main advances
// (same branch, new OID) while an agent resolution is in flight, resuming must stop and require an
// explicit retry rather than silently adopting the moved target — a fresh Preflight only compares
// branch *names*, so without an explicit OID check the movement would otherwise pass unnoticed.
func TestCmdWorktreeMergeResumeStopsOnTargetMovedSinceResolutionStarted(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	// Divergent edit to the same file on both sides -> a predicted conflict Preflight will report.
	mergeFixtureCLIWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureCLIWrite(t, repo, "f.txt", "main line\n", "main edits f")

	r, err := worktree.ResolveRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := worktree.Preflight(r, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Code != worktree.IssueConflict {
		t.Fatalf("Issues = %v, want exactly one conflict", plan.Issues)
	}
	mainBeforeResume := plan.TargetOID

	// Simulate an agent having already resolved the conflict and committed it in the worktree,
	// ancestor-correct over both pinned OIDs (CheckResolution's own contract).
	mergeFixtureCLIWrite(t, wt, "f.txt", "resolved\n", "agent resolves f")
	mergeFixtureCLIRun(t, wt, "merge", "--no-edit", "-s", "ours", plan.TargetOID) // records TargetOID as a merged parent

	op := &worktree.Operation{
		ID:                "op-target-moved",
		Repo:              r.Main,
		Worktree:          "feature",
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		Phase:             "resolving",
		StartedAt:         time.Now(),
		Deadline:          time.Now().Add(10 * time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := worktree.SaveOperation(op); err != nil {
		t.Fatal(err)
	}

	// Main moves on (same branch, new commit) while the resolution was supposedly in flight.
	mergeFixtureCLIWrite(t, repo, "unrelated.txt", "main moved on\n", "main advances during resolution")
	movedMain := cliHeadOf(t, repo)
	if movedMain == mainBeforeResume {
		t.Fatal("fixture invariant broken: main must have actually moved")
	}

	code, _, errb := captureOutput(t, func() int {
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

// TestCmdWorktreeMergeResumeMissingWorktreeFails covers an operation that names a worktree no longer
// on disk (removed, or never existed under this name) — --resume must fail with a clear reason rather
// than attempting to poll a path that isn't there.
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

	code, _, errb := captureOutput(t, func() int {
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
