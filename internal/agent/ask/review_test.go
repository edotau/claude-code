package ask

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	// exec keeps the hung git at one PID, so cancellation cannot leave a child holding its output pipes open.
	if err := os.WriteFile(git, []byte("#!/bin/sh\ncase \"$3\" in diff) exec sleep 125;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(t.TempDir())
	orig := reviewDiffTimeout
	reviewDiffTimeout = time.Second
	t.Cleanup(func() { reviewDiffTimeout = orig })
	start := time.Now()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Review("codex", nil, ReviewOptions{Uncommitted: true})
	})
	elapsed := time.Since(start)
	if code != 1 || !strings.Contains(stderr, "diff HEAD: signal: killed") {
		t.Fatalf("hung diff: exit %d after %v, stderr %q; want cancelled git diff", code, elapsed, stderr)
	}
	if elapsed < reviewDiffTimeout || elapsed > reviewDiffTimeout*3/2 {
		t.Fatalf("hung diff took %v; want cancellation within %v of %v", elapsed, reviewDiffTimeout/2, reviewDiffTimeout)
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
