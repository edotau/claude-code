package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// resolveFixtureConflictSetup builds the same divergent-edit-to-the-same-file conflict preflight_test.go
// exercises (TestPreflightPredictsTextConflict's shape), then runs Preflight so the caller gets back a
// plan with exactly one IssueConflict ready to hand to ResolveWithAgent.
func resolveFixtureConflictSetup(t *testing.T) (repo *Repo, main, wt string, plan *MergePlan) {
	t.Helper()
	repo, main, wt = mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main line\n", "main edits f")
	p, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(p.Issues) != 1 || p.Issues[0].Code != IssueConflict {
		t.Fatalf("Issues = %v, want exactly one %s", p.Issues, IssueConflict)
	}
	return repo, main, wt, p
}

// resolveFixtureFakeClock replaces nowFn/sleepFn with a deterministic in-memory clock (sleepFn just
// advances it) and shrinks pollInterval/maxResolveWait, so every test below runs in milliseconds with
// no real sleeping and no real model call — restoring every package var on cleanup.
func resolveFixtureFakeClock(t *testing.T, maxWait time.Duration) {
	t.Helper()
	origNow, origSleep, origPoll, origMax := nowFn, sleepFn, pollInterval, maxResolveWait
	fake := time.Now()
	nowFn = func() time.Time { return fake }
	sleepFn = func(d time.Duration) { fake = fake.Add(d) }
	pollInterval = time.Millisecond
	maxResolveWait = maxWait
	t.Cleanup(func() {
		nowFn, sleepFn, pollInterval, maxResolveWait = origNow, origSleep, origPoll, origMax
	})
}

// resolveFixtureFakeAgent installs fn as the one seam ResolveWithAgent calls instead of a real
// `claude-code ask`, restoring the real var on cleanup.
func resolveFixtureFakeAgent(t *testing.T, fn func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error)) {
	t.Helper()
	orig := runResolveAgent
	runResolveAgent = fn
	t.Cleanup(func() { runResolveAgent = orig })
}

func resolveFixtureRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	mergeFixtureRun(t, dir, args...)
}

// TestResolveWithAgentEditOnlyMarkersRemovedControllerCommits is the fresh-path happy case for the
// new resolve contract: a sandboxed agent (e.g. codex's workspace-write) can't write inside .git, so
// it only edits the conflicted file — finalizeAgentResolution is the one that stages and commits once
// the agent exits successfully with no leftover markers.
func TestResolveWithAgentEditOnlyMarkersRemovedControllerCommits(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		// Only an edit — no git add/commit, matching the prompt's instruction to a sandboxed agent.
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	if result == nil || result.Phase != "integrate" || result.AfterOID != headOf(wt) {
		t.Fatalf("result = %+v, want integrate -> %s", result, headOf(wt))
	}
	if op.Phase != "resolved" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "resolved")
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if mh {
		t.Fatal("MERGE_HEAD still set after the controller's own commit")
	}
	got, readErr := os.ReadFile(filepath.Join(wt, "f.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "resolved\n" {
		t.Fatalf("f.txt = %q, want the agent's edit %q", got, "resolved\n")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentUntrackedResidueIgnoredAndPromotes covers a plugin/hook leaving an UNTRACKED
// file behind in the worktree (e.g. a cache file like .impeccable/hook.cache.json) outside the
// conflicted paths: changedPaths now lists tracked changes only (--untracked-files=no), so
// finalizeAgentResolution never stages it, never refuses the commit over it, and the resolution still
// promotes — the residue is simply left on disk, untouched, exactly as the agent's tooling left it.
func TestResolveWithAgentUntrackedResidueIgnoredAndPromotes(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Untracked tooling residue, never `git add`-ed by anything — not one of the conflicted paths.
		if err := os.WriteFile(filepath.Join(workDir, "hook.cache.json"), []byte(`{"residue":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: want the untracked residue to be ignored, got error: %v", err)
	}
	if result == nil || result.Phase != "integrate" {
		t.Fatalf("result = %+v, want a completed integrate", result)
	}
	if op.Phase != "resolved" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "resolved")
	}
	// The residue itself must be left exactly as the agent's tooling wrote it — never staged, never removed.
	got, readErr := os.ReadFile(filepath.Join(wt, "hook.cache.json"))
	if readErr != nil {
		t.Fatalf("residue file was removed or never written: %v", readErr)
	}
	if string(got) != `{"residue":true}` {
		t.Fatalf("hook.cache.json = %q, want it untouched", got)
	}
	staged, err := runGit(context.Background(), wt, nil, "status", "--porcelain", "--", "hook.cache.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(staged)), "??") {
		t.Fatalf("git status for hook.cache.json = %q, want it still untracked (\"??\"), never staged", staged)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestCleanRefusesWhileUntrackedResidueExists covers the other half of the untracked-residue change:
// Clean's own dirty check (Inspect, --untracked-files=all) is unchanged, so residue left behind in a
// worktree still blocks an ordinary `worktree clean` — the two checks are deliberately different (the
// resolve controller's commit scope vs. Clean's "is this worktree empty-handed" bar) and this pins
// that they stay different. Built on a clean fast-forward merge (not a resolve) to isolate Clean's own
// dirty check from IntegrateTarget's — see the KNOWN DEFECT noted in this file's package comment: a
// residue file left behind by a real agent resolution currently makes the production merge's own
// post-resolution IntegrateTarget call refuse with IssueDirtyWorktree, which this test deliberately
// does not exercise.
func TestCleanRefusesWhileUntrackedResidueExists(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Preflight: issues=%v err=%v", plan.Issues, err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}
	if _, err := Promote(context.Background(), repo, plan); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Simulate tooling residue (e.g. a plugin hook cache) left behind after the merge already landed.
	if err := os.WriteFile(filepath.Join(wt, "hook.cache.json"), []byte(`{"residue":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanResult, err := Clean(repo, "feature", false)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if cleanResult.Removed {
		t.Fatal("Clean: want it refused while untracked residue remains, not removed")
	}
	if !strings.Contains(cleanResult.Reason, "uncommitted") {
		t.Fatalf("cleanResult.Reason = %q, want it to name the uncommitted/dirty state", cleanResult.Reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a refused clean: %v", err)
	}
}

// TestResolveWithAgentMarkersRemainFailsLikeTimeoutPreservingState covers the agent exiting without
// actually clearing every conflict marker: finalizeAgentResolution's conflictMarkersClear must refuse
// to stage or commit, failing the operation outright (no further agent turn is coming) while
// preserving the operation file and the agent's in-progress edits for a later --resume.
func TestResolveWithAgentMarkersRemainFailsLikeTimeoutPreservingState(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	const markeredEdit = "<<<<<<< HEAD\nmain line\n=======\nfeature line\n>>>>>>> feature\n"
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte(markeredEdit), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error — leftover conflict markers must never be staged or committed")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on a markers-remain failure", result)
	}
	if !strings.Contains(err.Error(), "conflict markers remain") {
		t.Fatalf("err = %q, want a markers-remain message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	// The operation file itself must be preserved (not deleted) and reflect the same terminal state.
	reloaded, loadErr := LoadOperation(repo, "feature")
	if loadErr != nil {
		t.Fatalf("LoadOperation after failure: %v", loadErr)
	}
	if reloaded.Phase != "failed" || reloaded.ID != op.ID {
		t.Fatalf("reloaded operation = %+v, want the preserved failed record", reloaded)
	}
	// The agent's edit must be preserved on disk — no `git merge --abort` or checkout on our part.
	got, readErr := os.ReadFile(filepath.Join(wt, "f.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != markeredEdit {
		t.Fatalf("f.txt = %q, want the agent's preserved marker-laden edit %q", got, markeredEdit)
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up after a markers-remain failure — no automatic cleanup is permitted")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentEditsUnrelatedFileRefused covers the agent touching a file outside the
// conflicted paths it was handed: finalizeAgentResolution must refuse to stage or commit anything,
// even though the conflicted file itself was correctly resolved.
func TestResolveWithAgentEditsUnrelatedFileRefused(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	// A pre-existing TRACKED file, committed before the conflict is ever materialized — distinct from
	// an untracked residue file (which the controller now ignores; see
	// TestResolveWithAgentUntrackedResidueIgnoredAndPromotes), so this pins the refusal to a change
	// git itself already tracks, not merely a new file on disk.
	mergeFixtureWrite(t, wt, "tracked.txt", "original\n", "add tracked file")
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "tracked.txt"), []byte("should not change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error — an edit outside the conflicted paths must be refused")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil when an out-of-scope edit is refused", result)
	}
	if !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("err = %q, want the out-of-scope message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up despite the out-of-scope refusal — no automatic cleanup is permitted")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentCleanAbortNeverCommittedByController covers the agent running `git merge
// --abort` despite the prompt telling it not to run any git command: the worktree reverts to a clean,
// unmerged state, so finalizeAgentResolution's own commit has nothing staged to commit and fails —
// the abort is never mistaken for, or silently turned into, a resolved state.
func TestResolveWithAgentCleanAbortNeverCommittedByController(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	sourceBefore := plan.SourceOID
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		return resolveAgentResult{}, nil
	})

	op, _, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a clean abort")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %q, want a commit-failure message (nothing left to stage after the abort)", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if headOf(wt) != sourceBefore {
		t.Fatalf("worktree HEAD = %s, want unchanged %s after abort", headOf(wt), sourceBefore)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentStaleMergeCommitNeverPromoted covers an agent that fabricates its own commit
// (ignoring the instruction not to run git commands): finalizeAgentResolution's own commit attempt
// finds nothing staged (the agent's commit already cleared the working tree) and fails — the
// fabricated commit is never promoted past this controller, by construction, regardless of its
// ancestry. (A human can still --resume: that path polls CheckResolution directly instead of trying
// to re-commit, and rejects the stale commit there — see TestResolveWithAgentResumeRevalidatesAndCompletes
// and the ancestry check in CheckResolution.)
func TestResolveWithAgentStaleMergeCommitNeverPromoted(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		resolveFixtureRunGit(t, workDir, "commit", "--allow-empty", "-m", "stale leftover commit")
		return resolveAgentResult{}, nil
	})

	op, _, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a stale/unrelated commit")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %q, want a commit-failure message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if ok, aerr := isAncestor(context.Background(), wt, plan.TargetOID, headOf(wt)); aerr == nil && ok {
		t.Fatal("fixture invariant broken: TargetOID must NOT be an ancestor of the stale commit")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentCommitsItselfFailsFreshButResumeSucceeds covers the documented fallback for an
// agent that commits on its own despite the instruction not to (ResolveWithAgent's doc comment): the
// FRESH run fails (finalizeAgentResolution's own re-commit finds nothing staged), but the operation
// record is preserved with AgentExitOK — a later --resume (the op != nil / polling path) never
// re-invokes the agent and instead revalidates the already-committed state directly via
// CheckResolution, succeeding without ever trying to commit again.
func TestResolveWithAgentCommitsItselfFailsFreshButResumeSucceeds(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resolveFixtureRunGit(t, workDir, "add", "f.txt")
		resolveFixtureRunGit(t, workDir, "commit", "--no-edit")
		return resolveAgentResult{}, nil
	})

	failedOp, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent (fresh): want an error — the controller's own commit has nothing staged")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on the fresh-run failure", result)
	}
	if failedOp.Phase != "failed" {
		t.Fatalf("failedOp.Phase = %q, want %q", failedOp.Phase, "failed")
	}
	if !failedOp.AgentExitOK {
		t.Fatal("failedOp.AgentExitOK = false, want true — the agent process itself exited cleanly")
	}
	resolvedHead := headOf(wt) // the agent's own commit, already in place

	// --resume: never re-invokes the agent (the fake would fail the test if called again).
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		t.Fatal("agent must never be re-invoked on --resume")
		return resolveAgentResult{}, fmt.Errorf("unreachable")
	})
	resumedOp, resumeResult, resumeErr := ResolveWithAgent(context.Background(), repo, plan, "claude", failedOp)
	if resumeErr != nil {
		t.Fatalf("ResolveWithAgent (resume): %v", resumeErr)
	}
	if resumeResult == nil || resumeResult.Phase != "integrate" || resumeResult.AfterOID != resolvedHead {
		t.Fatalf("resumeResult = %+v, want integrate -> %s", resumeResult, resolvedHead)
	}
	if resumedOp.Phase != "resolved" {
		t.Fatalf("resumedOp.Phase = %q, want %q", resumedOp.Phase, "resolved")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentResumeRevalidatesAndCompletes simulates a crash: an operation was saved and the
// conflict materialized, but the controller process died before ever polling. The resolution — done by
// the agent before the crash — is already a clean, committed, ancestor-correct merge on disk. --resume
// (op != nil) must revalidate that state and report success WITHOUT re-invoking the agent.
func TestResolveWithAgentResumeRevalidatesAndCompletes(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)

	materialized, err := materializeConflict(context.Background(), plan)
	if err != nil {
		t.Fatalf("materializeConflict: %v", err)
	}
	if materialized.Phase != "conflict" {
		t.Fatalf("materialized.Phase = %q, want %q", materialized.Phase, "conflict")
	}

	op := &Operation{
		ID:                "op-crash-sim",
		Repo:              repo.Main,
		Worktree:          filepath.Base(wt),
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		Phase:             "resolving",
		StartedAt:         time.Now(),
		Deadline:          time.Now().Add(10 * time.Minute),
		Agent:             "claude",
		AgentExitOK:       true, // the agent itself returned success, fallback-free, before the crash
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}

	// Simulate the agent having actually finished the resolution before the crash.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved by agent before crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolveFixtureRunGit(t, wt, "add", "f.txt")
	resolveFixtureRunGit(t, wt, "commit", "--no-edit")
	resolvedHead := headOf(wt)

	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		t.Fatal("agent must never be re-invoked on --resume")
		return resolveAgentResult{}, fmt.Errorf("unreachable")
	})

	gotOp, result, err := ResolveWithAgent(context.Background(), repo, plan, op.Agent, op)
	if err != nil {
		t.Fatalf("ResolveWithAgent (resume): %v", err)
	}
	if result == nil || result.Phase != "integrate" || result.AfterOID != resolvedHead {
		t.Fatalf("result = %+v, want integrate -> %s", result, resolvedHead)
	}
	if gotOp.Phase != "resolved" {
		t.Fatalf("gotOp.Phase = %q, want %q", gotOp.Phase, "resolved")
	}
	reloaded, loadErr := LoadOperation(repo, "feature")
	if loadErr != nil || reloaded.Phase != "resolved" {
		t.Fatalf("reloaded = %+v, err=%v, want a persisted resolved state", reloaded, loadErr)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentThenPromoteStopsWhenTargetAdvanced chains a successful agent resolution into
// Promote, then advances main before Promote runs — reproducing "target moved between resolve and
// promote". Promote must stop with a retry hint rather than falling back to a real conflicting merge.
func TestResolveWithAgentThenPromoteStopsWhenTargetAdvanced(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		// Edit only — the controller (finalizeAgentResolution) stages and commits once the agent exits.
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	_, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	if result.Phase != "integrate" {
		t.Fatalf("result.Phase = %q, want %q", result.Phase, "integrate")
	}
	if result.AfterOID != headOf(wt) {
		t.Fatalf("result.AfterOID = %s, want the resolved worktree HEAD %s", result.AfterOID, headOf(wt))
	}

	// Matches production order (cmdWorktreeMerge): IntegrateTarget always runs after resolution,
	// pinning plan.IntegratedOID — content is already merged, so this is a no-op re-merge.
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	// Main moves on after resolution but before Promote — plan.TargetOID is now stale.
	mergeFixtureWrite(t, main, "unrelated.txt", "main moved on\n", "main advances independently")
	advancedMain := headOf(main)

	promoted, promoteErr := Promote(context.Background(), repo, plan)
	if promoteErr == nil {
		t.Fatal("Promote: want an error — target advanced since the plan was pinned")
	}
	if promoted == nil || promoted.ResumeHint == "" {
		t.Fatalf("promoted = %+v, want a non-empty ResumeHint", promoted)
	}
	if !strings.Contains(promoteErr.Error(), "target moved") {
		t.Fatalf("err = %q, want a target-moved message", promoteErr)
	}
	if headOf(main) != advancedMain {
		t.Fatalf("main HEAD = %s, want unchanged %s (Promote must not fall back to a real merge)", headOf(main), advancedMain)
	}
}

// TestWithWorktreeLeaseRefusesWhileHeldBeforeResolveRuns is the regression test for Appendix A
// codex-review finding #3's lease half (internal/worktree/resolve.go:134), updated for the lease
// having moved to the caller's responsibility (ResolveWithAgent itself "does no locking of its own"
// per its own doc comment): a concurrent resolve/resume/merge/clean for the same worktree must never
// run alongside a still-active one — WithWorktreeLease must refuse outright, and its fn (here wrapping
// ResolveWithAgent, the way cmdWorktreeMerge and Merge both call it) must never run at all.
func TestWithWorktreeLeaseRefusesWhileHeldBeforeResolveRuns(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)

	if err := os.MkdirAll(operationsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(operationLeasePath(repo.Main, filepath.Base(wt)), 2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	resolveWasCalled := false
	err := WithWorktreeLease(repo, filepath.Base(wt), func() error {
		resolveWasCalled = true
		_, _, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
		return err
	})
	if err == nil {
		t.Fatal("WithWorktreeLease: want an error while another operation holds the lease")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("err = %q, want it to report the lease conflict", err)
	}
	if resolveWasCalled {
		t.Fatal("ResolveWithAgent ran despite the lease being held — the lease refusal must happen before fn runs")
	}
	// Refused before ever touching the worktree: no conflict was materialized, main stayed clean.
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if mh {
		t.Fatal("a conflict was materialized despite the lease refusal")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentExplicitCodexFallenBackToClaudeIsMismatch covers an explicitly selected Codex
// leg that silently ran as Claude instead (the agent runner's own fallback) — ResolveWithAgent must
// stop and report the mismatch rather than accept a different model's resolution as the selected one.
func TestResolveWithAgentExplicitCodexFallenBackToClaudeIsMismatch(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if agentName != "codex" {
			t.Fatalf("agentName = %q, want codex", agentName)
		}
		return resolveAgentResult{Fallback: "codex"}, nil
	})

	op, _, err := ResolveWithAgent(context.Background(), repo, plan, "codex", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want a mismatch error on fallback")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "fell back") {
		t.Fatalf("err = %q, want it to name the fallback mismatch", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up on a fallback mismatch — no automatic cleanup is permitted")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}
