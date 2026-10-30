package cli

import (
	"os"
	"strings"
	"testing"
)

// TestCmdWorktreeMergeNoDetectableGateRefusesWithoutNoVerify covers the default (no --verify, no
// --no-verify) with nothing to auto-detect in the worktree: `worktree merge` must refuse outright
// before integrating anything, leaving both main and the worktree untouched — --no-verify is the
// only way around it.
func TestCmdWorktreeMergeNoDetectableGateRefusesWithoutNoVerify(t *testing.T) {
	repo := mergeFixtureCLISetup(t)
	wt := mergeFixtureCLIWorktreePath(t, repo)
	mergeFixtureCLIWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := cliHeadOf(t, repo)

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "feature"}) })
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

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "--no-verify", "feature"}) })
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

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "feature"}) })
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

	code, out, errb := captureOutput(t, func() int { return cmdWorktree([]string{"merge", "feature"}) })
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

	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--verify", "true", "feature"})
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

	code, out, errb := captureOutput(t, func() int {
		return cmdWorktree([]string{"merge", "--verify", "false | cat", "feature"})
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
