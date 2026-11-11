package ask

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
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
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"SHIP IT"}}'`)
	parent := agent.NewRecord("opencode", agent.RunOptions{Prompt: "review"}, time.Now())
	if err := parent.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(reviewJobEnv, parent.ID)
	run := func() int {
		return runSplitReview("opencode", nil, []Part{{Diff: []byte("test diff")}},
			Scope{Kind: "uncommitted"}, ReviewOptions{})
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "SHIP IT") {
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

func TestCaptureAnswerClosesPipe(t *testing.T) {
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("requires /proc/self/fd")
	}
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	want := strings.Repeat("review answer\n", 8192)
	for range 16 {
		got, code := captureAnswer(func() int {
			fmt.Fprint(os.Stdout, want)
			return 7
		})
		if got != want || code != 7 {
			t.Fatalf("capture: bytes=%d, code=%d; want bytes=%d, code=7", len(got), code, len(want))
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before) {
		t.Fatalf("capture leaked descriptors: before=%d after=%d", len(before), len(after))
	}
}

// Given a dispatch dir that rejects writes, Then a detached split review fails instead of spawning a child with no record.
