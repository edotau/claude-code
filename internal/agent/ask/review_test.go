package ask

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestReviewAuthorModelUsesInvokingDispatchThenSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_MODEL", "claude-opus-5-5")
	t.Setenv(proc.DispatchIDEnv, "")
	if got := reviewAuthorModel(); got != "claude-opus-5-5" {
		t.Fatalf("session model = %q", got)
	}
	r := agent.Record{ID: "20261117T120000-copilot", Model: "gpt-6.1-sol"}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(proc.DispatchIDEnv, r.ID)
	if got := reviewAuthorModel(); got != r.Model {
		t.Fatalf("dispatch model = %q, want %q", got, r.Model)
	}
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv("CLAUDE_MODEL", "")
	if got := reviewAuthorModel(); got != "" {
		t.Fatalf("unknown author model = %q", got)
	}
}

func captureFailedStdout(t *testing.T, run func() int) (int, string) {
	t.Helper()
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	}()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	return code, stderr
}

func requireOutputFailure(t *testing.T, code int, stderr, diagnostic string) {
	t.Helper()
	if code != 1 || !strings.Contains(stderr, diagnostic) || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost output: exit %d, stderr %q; want exit 1 with %q and write error", code, stderr, diagnostic)
	}
}

func TestSplitReviewReportsMergedWriteFailure(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.SetRecord(t, "opencode", "profile", "zen-free")
	t.Setenv("OPENCODE_API_KEY", "test-key")
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"SHIP"}}'`)
	parent := agent.NewRecord("opencode", agent.RunOptions{Prompt: "review"}, time.Now())
	if err := parent.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(reviewJobEnv, parent.ID)
	run := func() int {
		p, err := reviewParent()
		if err != nil {
			t.Fatal(err)
		}
		return runSplitReview("opencode", nil, []part{{Diff: []byte("test diff")}}, ReviewOptions{}, p)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "SHIP") {
		t.Fatalf("review control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stderr = captureFailedStdout(t, run)
	requireOutputFailure(t, code, stderr, "review: write merged report:")
	got, err := agent.LoadRecord(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || !strings.Contains(got.Error, "write merged report:") || got.Progress != "" {
		t.Fatalf("lost merged review marked successful: %+v", got)
	}
}

func TestReviewWithParentJobClosesRecordOnEarlyExit(t *testing.T) {
	vaulttest.UseFreshKey(t)
	parent := agent.NewRecord("opencode", agent.RunOptions{Prompt: "review"}, time.Now())
	if err := parent.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(reviewJobEnv, parent.ID)

	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Review("opencode", nil, ReviewOptions{Uncommitted: true, Base: "main"})
	})
	if code != 1 || !strings.Contains(stderr, "at most one of") {
		t.Fatalf("Review exit %d, stderr %q; want conflicting-scope error", code, stderr)
	}
	got, err := agent.LoadRecord(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || !strings.Contains(got.Error, "at most one of") || got.Progress != "" {
		t.Fatalf("early-exit job record = %+v; want error, scope diagnostic, empty progress", got)
	}
}

func TestAgentReviewBoundsDiffPreparation(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh script")
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	// exec keeps the hung git at one PID, so cancellation cannot leave a child holding its output pipes open.
	if err := os.WriteFile(git, []byte("#!/bin/sh\ncase \"$3\" in diff) exec sleep 125;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(t.TempDir())
	orig := reviewDiffTimeout
	reviewDiffTimeout = 600 * time.Millisecond
	t.Cleanup(func() { reviewDiffTimeout = orig })
	start := time.Now()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Review("codex", nil, ReviewOptions{Uncommitted: true})
	})
	elapsed := time.Since(start)
	// the deadline may land on an earlier git call under load, so any cancelled call proves the bound
	if code != 1 || !strings.Contains(stderr, "signal: killed") && !strings.Contains(stderr, "context deadline exceeded") {
		t.Fatalf("hung diff: exit %d after %v, stderr %q; want cancelled git", code, elapsed, stderr)
	}
	if elapsed < reviewDiffTimeout {
		t.Fatalf("hung diff ended after %v, before its %v deadline", elapsed, reviewDiffTimeout)
	}
}

func TestCaptureAnswerSharesNoState(t *testing.T) {
	stdout := os.Stdout
	var wg sync.WaitGroup
	for i := range 8 {
		want := strings.Repeat(fmt.Sprintf("answer %d\n", i), 4096)
		wg.Go(func() {
			got, code := captureAnswer(func(out io.Writer) int {
				fmt.Fprint(out, want)
				return i
			})
			if got != want || code != i {
				t.Errorf("capture %d: bytes=%d, code=%d; want bytes=%d, code=%d", i, len(got), code, len(want), i)
			}
		})
	}
	wg.Wait()
	if os.Stdout != stdout {
		t.Fatal("captureAnswer replaced os.Stdout")
	}
}

// Given a dispatch dir that rejects writes, a detached split review fails rather than spawn a child with no record.

func TestSplitReviewParentUsageSummed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	parent := agent.NewRecord("codex", agent.RunOptions{Prompt: "review"}, time.Now())
	parts := []wire.Usage{
		{InputTokens: 10, OutputTokens: 3, CacheCreationTokens: 4, CacheReadTokens: 5, CostUSD: .125},
		{InputTokens: 20, OutputTokens: 7, CacheCreationTokens: 6, CacheReadTokens: 8, CostUSD: .25},
	}
	original := reviewAskCmd
	t.Cleanup(func() { reviewAskCmd = original })
	i := 0
	reviewAskCmd = func(args []string, out io.Writer) int {
		if i >= len(parts) {
			t.Fatal("extra part")
		}
		err := json.NewEncoder(out).Encode(wire.NewEnvelope(wire.Result{Answer: "SHIP", Usage: parts[i]}, nil, "part"))
		if err != nil {
			t.Fatal(err)
		}
		i++
		return 0
	}
	rc, _, stderr := hosttest.CaptureOutput(t, func() int {
		return runSplitReview("codex", nil, []part{{Diff: []byte("one")}, {Diff: []byte("two")}}, ReviewOptions{}, &parent)
	})
	if rc != 0 || i != 2 {
		t.Fatalf("exit=%d parts=%d stderr=%s", rc, i, stderr)
	}
	got, err := agent.LoadRecord(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.Usage{InputTokens: 30, OutputTokens: 10, CacheCreationTokens: 10, CacheReadTokens: 13, CostUSD: .375}
	if got.Usage == nil || *got.Usage != want {
		t.Fatalf("usage=%+v want=%+v", got.Usage, want)
	}
}

func TestSingleReviewChildParentUsageSummed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	parent := agent.NewRecord("codex", agent.RunOptions{Prompt: "review"}, time.Now())
	original := reviewAskCmd
	t.Cleanup(func() { reviewAskCmd = original })
	reviewAskCmd = func(args []string, out io.Writer) int {
		env := wire.NewEnvelope(wire.Result{Answer: "SHIP", Usage: wire.Usage{InputTokens: 9, OutputTokens: 2}}, nil, "part")
		if err := json.NewEncoder(out).Encode(env); err != nil {
			t.Fatal(err)
		}
		return 0
	}
	rc, out, _ := hosttest.CaptureOutput(t, func() int {
		return runSingleReview("codex", []byte("diff"), ReviewOptions{}, &parent, time.Now())
	})
	got, err := agent.LoadRecord(parent.ID)
	if err != nil || rc != 0 || !strings.Contains(out, "SHIP") || got.Usage == nil || got.Usage.InputTokens != 9 {
		t.Fatalf("rc=%d out=%q usage=%+v err=%v", rc, out, got.Usage, err)
	}
}

// Only overlay cloud on claude reaches ultrareview; every other review takes the local path or is refused.
func TestReviewLaneCloudGate(t *testing.T) {
	vaulttest.Use(t)
	log := filepath.Join(t.TempDir(), "argv")
	pathstest.FakeVendor(t, "claude", `echo "$*" >> `+log)
	open := `{"cloud":true}`
	tests := []struct {
		name, agent, flag, env string
		nested                 bool
		wantDone               bool
		wantRC                 int
		ran                    string
	}{
		{"no overlay", "claude", "", "", false, false, 0, ""},
		{"overlay without cloud in env", "claude", "", `{"maxChildren":1}`, false, false, 0, ""},
		{"cloud by flag", "claude", open, "", false, true, 0, "ultrareview main"},
		{"cloud by env", "claude", "", open, false, true, 0, "ultrareview main"},
		{"cloud nested", "claude", open, "", true, true, 1, ""},
		{"cloud on copilot", "copilot", open, "", false, true, 1, ""},
		{"cloud on codex stays local", "codex", "", open, false, false, 0, ""},
		{"flag without cloud", "claude", `{"maxChildren":1}`, "", false, true, 1, ""},
		{"databricks", "claude", `{"cloud":true,"provider":"databricks"}`, "", false, true, 1, ""},
	}
	for _, tc := range tests {
		os.Remove(log)
		t.Setenv(agentconfig.OverlayVar, tc.env)
		id := ""
		if tc.nested {
			id = "d1"
		}
		t.Setenv(proc.DispatchIDEnv, id)
		rc, done := reviewLane(tc.agent, ReviewOptions{Base: "main", Overlay: tc.flag})
		b, _ := os.ReadFile(log)
		if rc != tc.wantRC || done != tc.wantDone || strings.TrimSpace(string(b)) != tc.ran {
			t.Errorf("%s: rc=%d done=%v ran=%q, want rc=%d done=%v ran=%q", tc.name, rc, done, b, tc.wantRC, tc.wantDone, tc.ran)
		}
	}
}
