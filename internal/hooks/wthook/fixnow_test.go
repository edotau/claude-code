package wthook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// fixNowEnv is a main interactive session with a fresh failure log; it returns the session's checkout.
func fixNowEnv(t *testing.T) string {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_KEYRING", "off")
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(proc.DispatchDepthEnv, "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv(fixNowIdleEnv, "")
	return hosttest.Repo(t)
}

func reportFail(t *testing.T, kind worktree.FailureKind, subject string) string {
	t.Helper()
	if _, err := worktree.ReportFailure(worktree.FailuresDir(), worktree.Failure{Kind: kind, Subject: subject, Summary: "boom"}); err != nil {
		t.Fatal(err)
	}
	return worktree.FailureID(kind, subject)
}

// TestFixNowModesOnce: idle text at Stop, present text after a prompt, once per session with no plain repeat.
func TestFixNowModesOnce(t *testing.T) {
	cwd := fixNowEnv(t)
	reportFail(t, worktree.FailAudit, "x")
	idle := FixNow(&core.Input{SessionID: "sB", CWD: cwd})
	for _, want := range []string{"fix now: audit x: boom; claim: claude-code failures claim ", "apply the recommended fix now",
		"-autofix-audit-x.md", "owner idle: no prompt for 10m0s", "adds a restriction waits for the owner"} {
		if idle.Decision != core.Block || !strings.Contains(idle.Reason, want) {
			t.Fatalf("idle stop = %+v, want a continuation with %q", idle, want)
		}
	}
	if again := FixNow(&core.Input{SessionID: "sB", CWD: cwd}); again.Decision != core.Allow {
		t.Fatalf("second stop = %+v, want nothing (once per session)", again)
	}
	if next := Busy(&core.Input{SessionID: "sB", CWD: cwd, Prompt: "ok"}); strings.Contains(next.Context, "audit x") {
		t.Fatalf("prompt after the stop = %q, want no plain repeat of x", next.Context)
	}
	reportFail(t, worktree.FailLaneGate, "y")
	present := Busy(&core.Input{SessionID: "sA", CWD: cwd, Prompt: "hello"})
	if !strings.Contains(present.Context, "fix now: lane_gate y") || !strings.Contains(present.Context, "give the owner options") ||
		strings.Contains(present.Context, "fix now: audit x") {
		t.Fatalf("present prompt = %q, want only y's present directive", present.Context)
	}
}

// TestFixNowClaimedGetsNothing: a claimed entry is never a directive.
func TestFixNowClaimedGetsNothing(t *testing.T) {
	cwd := fixNowEnv(t)
	if _, err := worktree.ClaimFailure(worktree.FailuresDir(), reportFail(t, worktree.FailContract, "z"), "peer"); err != nil {
		t.Fatal(err)
	}
	if got := FixNow(&core.Input{SessionID: "sC", CWD: cwd}); strings.Contains(got.Reason, "contract z") {
		t.Fatalf("claimed stop = %+v, want no directive", got)
	}
}

// TestFixNowLeafGetsNothing: a subagent payload or a dispatched process never gets the directive.
func TestFixNowLeafGetsNothing(t *testing.T) {
	cwd := fixNowEnv(t)
	reportFail(t, worktree.FailLaneGate, "lane")
	if got := FixNow(&core.Input{SessionID: "sL", CWD: cwd, AgentID: "a1"}); got.Decision != core.Allow {
		t.Fatalf("subagent stop = %+v, want nothing", got)
	}
	codex := core.ParseInputFor(strings.NewReader(`{"session_id":"sX","cwd":"`+cwd+`"}`), core.AgentCodex)
	if got := FixNow(codex); got.Decision != core.Allow {
		t.Fatalf("codex stop = %+v, want nothing", got)
	}
	t.Setenv(proc.DispatchIDEnv, "job")
	if got := FixNow(&core.Input{SessionID: "sL", CWD: cwd}); got.Decision != core.Allow {
		t.Fatalf("dispatched stop = %+v, want nothing", got)
	}
}

// TestFixNowThirdDeliveryIsPlainNotice: after DirectiveMax unclaimed deliveries a prompt gets the plain notice once.
func TestFixNowThirdDeliveryIsPlainNotice(t *testing.T) {
	cwd := fixNowEnv(t)
	id := reportFail(t, worktree.FailAudit, "spent")
	offer := filepath.Join(worktree.FailuresDir(), "claims", id+".offer")
	if err := os.MkdirAll(filepath.Dir(offer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(offer, []byte(`{"sessions":["s1","s2"],"time":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := worktree.FailuresSince(worktree.FailuresDir(), "s3", 10); err != nil { // the cursor's own notice is spent
		t.Fatal(err)
	}
	if got := FixNow(&core.Input{SessionID: "s3", CWD: cwd}); got.Decision != core.Allow {
		t.Fatalf("stop after two deliveries = %+v, want nothing", got)
	}
	got := Busy(&core.Input{SessionID: "s3", CWD: cwd, Prompt: "hi"}).Context
	if strings.Contains(got, "fix now") || !strings.Contains(got, "failure audit spent") || !strings.Contains(got, "claim: claude-code failures claim "+id) {
		t.Fatalf("third delivery = %q, want the plain notice and claim", got)
	}
	if again := Busy(&core.Input{SessionID: "s3", CWD: cwd, Prompt: "hi"}).Context; strings.Contains(again, "spent") {
		t.Fatalf("fourth delivery = %q, want nothing", again)
	}
	if un, _ := worktree.UnclaimedDirectives(t.Context(), worktree.FailuresDir()); len(un) != 1 {
		t.Fatalf("unclaimed directives = %d, want 1 for the doctor row", len(un))
	}
}
