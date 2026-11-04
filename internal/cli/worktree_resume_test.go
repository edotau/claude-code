package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/worktree"
)

// A clean plan must promote without dispatching --resolve-agent, even without an agent seam.
func TestCmdWorktreeMergeResolveAgentIgnoredOnCleanPlan(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceOID := cliHeadOf(t, wt)

	// --no-verify isolates agent dispatch from verification in a fixture without a detectable gate.
	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--no-verify", "--resolve-agent", "claude", "feature"})
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

// --resume without a saved operation must report LoadOperation's error and leave main untouched.
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

	code, _, errb := captureOutput(t, func() int {
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

	code, _, errb := captureOutput(t, func() int {
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
