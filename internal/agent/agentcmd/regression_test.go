package agentcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestDispatchReportWriteFailureExitsOne(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.SetRecord(t, "opencode", "profile", "zen-free")
	t.Setenv("OPENCODE_API_KEY", "test-key")
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"successful leg"}}'`)
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, format := range []string{helptext.Text, helptext.JSON} {
		t.Run(format, func(t *testing.T) {
			checkDispatchReportWriteFailure(t, full, format)
		})
	}
}

func checkDispatchReportWriteFailure(t *testing.T, full *os.File, format string) {
	t.Helper()
	role := roles.Role{Name: "explore", Agent: "opencode"}
	request := roles.Request{Task: "test report", Parts: []string{"test leg"}, WorkDir: t.TempDir()}
	run := func() int { return dispatchRun(t.Context(), role, request, 1, format, false) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "successful leg") {
		t.Fatalf("successful dispatch control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	if code != 1 || !strings.Contains(stderr, "dispatch: write report:") || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost report: exit %d, stderr %q; want exit 1 naming report write failure", code, stderr)
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
	start := time.Now()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return agentReview("codex", []string{"--uncommitted"})
	})
	elapsed := time.Since(start)
	if code != 1 || !strings.Contains(stderr, "diff HEAD: signal: killed") {
		t.Fatalf("hung diff: exit %d after %v, stderr %q; want cancelled git diff", code, elapsed, stderr)
	}
	if elapsed < reviewDiffTimeout || elapsed > reviewDiffTimeout+3*time.Second {
		t.Fatalf("hung diff took %v; want cancellation within 3s of %v", elapsed, reviewDiffTimeout)
	}
}
