package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeRejectsCommittedOutOfScopeEdits(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved\n")
		regressionWriteFile(t, filepath.Join(wt, "outside.txt"), "unauthorized\n")
		mergeFixtureRun(t, wt, "add", "f.txt", "outside.txt")
		mergeFixtureRun(t, wt, "commit", "--no-edit")
		return resolveAgentResult{}, nil
	})
	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("fresh resolution must reject committed scope violation: result=%+v err=%v", result, err)
	}
	if !op.AgentExitOK {
		t.Fatal("fixture must preserve the successful agent exit for resume")
	}
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		t.Fatal("resume must not invoke the agent")
		return resolveAgentResult{}, nil
	})
	_, result, err = ResolveWithAgent(context.Background(), repo, plan, "claude", op)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("resume accepted the same committed scope violation: result=%+v err=%v", result, err)
	}
}

func TestCheckResolutionRejectsCommittedConflictMarkers(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	op := crashedOperation(t, repo, wt, plan)
	mergeFixtureRun(t, wt, "add", "f.txt")
	mergeFixtureRun(t, wt, "commit", "--no-edit")
	state, reason, err := CheckResolution(context.Background(), op, wt)
	if err != nil || state != resolutionInvalid || !strings.Contains(reason, "conflict markers") {
		t.Fatalf("committed markers accepted: state=%v reason=%q err=%v", state, reason, err)
	}
}

func TestResolutionAllowsTrailingWhitespace(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (resolveAgentResult, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
		if out, err := runGit(context.Background(), wt, nil, "diff", "--check"); err == nil {
			t.Fatalf("fixture must trigger git's whitespace warning: %q", out)
		}
		return resolveAgentResult{}, nil
	})
	op, result, err := ResolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("whitespace without conflict markers must resolve: %v", err)
	}
	requirePhase(t, "operation", op.Phase, "resolved")
	requireIntegrate(t, result, headOf(wt))
	requireFileText(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
}

func regressionWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// a directory at the op path makes every save fail
