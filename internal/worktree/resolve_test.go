package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resolveFixtureConflictSetup builds the same divergent-edit-to-the-same-file conflict preflight_test.go
// exercises (TestPreflightPredictsTextConflict's shape), then runs Preflight so the caller gets back a
// plan with exactly one IssueConflict ready to hand to ResolveWithAgent.
func resolveFixtureConflictSetup(t *testing.T) (repo *Repo, main, wt string, plan *MergePlan) {
	t.Helper()
	repo, main, wt = mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	mergeFixtureWrite(t, main, "f.txt", "main line\n", "main edits f")
	p, err := Preflight(repo, "feature", "")
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
func resolveFixtureFakeAgent(t *testing.T, fn func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error)) {
	t.Helper()
	orig := runResolveAgent
	runResolveAgent = fn
	t.Cleanup(func() { runResolveAgent = orig })
}

func resolveFixtureRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	mergeFixtureRun(t, dir, args...)
}

// TestResolveWithAgentStagedButUncommittedIsNotSuccess is the one guard the whole poll loop exists
// for: `git add` on every conflicted path clears the unmerged-paths check, but MERGE_HEAD is still
// set until a commit lands. A fake agent that stages and stops must never be read as resolved.
func TestResolveWithAgentStagedButUncommittedIsNotSuccess(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, 10*time.Millisecond)
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resolveFixtureRunGit(t, workDir, "add", "f.txt")
		return resolveAgentResult{}, nil // agent reports success without ever committing
	})

	op, _, err := ResolveWithAgent(repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error — staged-but-uncommitted must never count as resolved")
	}
	if !strings.Contains(err.Error(), "no clean, committed resolution") {
		t.Fatalf("err = %q, want a timeout message", err)
	}
	if op.Phase != "timeout" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "timeout")
	}
	mh, mhErr := hasMergeHead(wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleared — the staged-only state must still read as an in-progress merge")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentCleanAbortIsRejected covers the agent running `git merge --abort`: HEAD reverts
// to exactly the original source OID, which CheckResolution must read as "aborted, not resolved",
// not as a no-op success.
func TestResolveWithAgentCleanAbortIsRejected(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	sourceBefore := plan.SourceOID
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		return resolveAgentResult{}, nil
	})

	op, _, err := ResolveWithAgent(repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a clean abort")
	}
	if !strings.Contains(err.Error(), "aborted, not resolved") {
		t.Fatalf("err = %q, want the abort message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if headOf(wt) != sourceBefore {
		t.Fatalf("worktree HEAD = %s, want unchanged %s after abort", headOf(wt), sourceBefore)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentStaleMergeCommitIsRejected covers the case an old aborted attempt (or an agent
// that fabricates an unrelated commit) leaves HEAD advanced without the target OID ever having been
// merged in — CheckResolution's ancestry check must still reject it as a stale/unrelated commit.
func TestResolveWithAgentStaleMergeCommitIsRejected(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		resolveFixtureRunGit(t, workDir, "commit", "--allow-empty", "-m", "stale leftover commit")
		return resolveAgentResult{}, nil
	})

	op, _, err := ResolveWithAgent(repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a stale/unrelated commit")
	}
	if !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("err = %q, want the stale-ancestry message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if ok, aerr := isAncestor(wt, plan.TargetOID, headOf(wt)); aerr == nil && ok {
		t.Fatal("fixture invariant broken: TargetOID must NOT be an ancestor of the stale commit")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestResolveWithAgentTimeoutPreservesOperationAndEdits: an agent that edits the conflicted file but
// never stages or commits must time out, and the timeout path must stop without any cleanup — the
// operation file and the agent's in-progress edits both survive untouched for a later --resume.
func TestResolveWithAgentTimeoutPreservesOperationAndEdits(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	const partialEdit = "half-resolved, never staged\n"
	resolveFixtureFakeClock(t, 6*time.Millisecond)
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte(partialEdit), 0o644); err != nil {
			t.Fatal(err)
		}
		return resolveAgentResult{}, nil
	})

	op, result, err := ResolveWithAgent(repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want a timeout error")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on timeout", result)
	}
	if !strings.Contains(err.Error(), "no clean, committed resolution") {
		t.Fatalf("err = %q, want a timeout message", err)
	}
	if op.Phase != "timeout" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "timeout")
	}
	// The operation file itself must be preserved (not deleted) and reflect the same terminal state.
	reloaded, loadErr := LoadOperation(repo, "feature")
	if loadErr != nil {
		t.Fatalf("LoadOperation after timeout: %v", loadErr)
	}
	if reloaded.Phase != "timeout" || reloaded.ID != op.ID {
		t.Fatalf("reloaded operation = %+v, want the preserved timeout record", reloaded)
	}
	// The agent's edit must be preserved on disk — no `git merge --abort` or checkout on our part.
	got, readErr := os.ReadFile(filepath.Join(wt, "f.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != partialEdit {
		t.Fatalf("f.txt = %q, want the agent's preserved partial edit %q", got, partialEdit)
	}
	mh, mhErr := hasMergeHead(wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up after timeout — no automatic cleanup is permitted")
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

	materialized, err := materializeConflict(plan)
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
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		t.Fatal("agent must never be re-invoked on --resume")
		return resolveAgentResult{}, fmt.Errorf("unreachable")
	})

	gotOp, result, err := ResolveWithAgent(repo, plan, op.Agent, op)
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
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resolveFixtureRunGit(t, workDir, "add", "f.txt")
		resolveFixtureRunGit(t, workDir, "commit", "--no-edit")
		return resolveAgentResult{}, nil
	})

	_, result, err := ResolveWithAgent(repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	if result.Phase != "integrate" {
		t.Fatalf("result.Phase = %q, want %q", result.Phase, "integrate")
	}
	if result.AfterOID != headOf(wt) {
		t.Fatalf("result.AfterOID = %s, want the resolved worktree HEAD %s", result.AfterOID, headOf(wt))
	}

	// Main moves on after resolution but before Promote — plan.TargetOID is now stale.
	mergeFixtureWrite(t, main, "unrelated.txt", "main moved on\n", "main advances independently")
	advancedMain := headOf(main)

	promoted, promoteErr := Promote(repo, plan)
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

// TestResolveWithAgentExplicitCodexFallenBackToClaudeIsMismatch covers an explicitly selected Codex
// leg that silently ran as Claude instead (the agent runner's own fallback) — ResolveWithAgent must
// stop and report the mismatch rather than accept a different model's resolution as the selected one.
func TestResolveWithAgentExplicitCodexFallenBackToClaudeIsMismatch(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
		if agentName != "codex" {
			t.Fatalf("agentName = %q, want codex", agentName)
		}
		return resolveAgentResult{Fallback: "codex"}, nil
	})

	op, _, err := ResolveWithAgent(repo, plan, "codex", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want a mismatch error on fallback")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "fell back") {
		t.Fatalf("err = %q, want it to name the fallback mismatch", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	mh, mhErr := hasMergeHead(wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up on a fallback mismatch — no automatic cleanup is permitted")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}
