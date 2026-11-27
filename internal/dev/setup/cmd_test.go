package setup

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/dev/worktree/worktreetest"
	"github.com/edotau/claude-code/internal/platform/hosttest"
)

func TestMain(m *testing.M) {
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	agent.NoticeOut = io.Discard
	os.Exit(hosttest.Main(m))
}

// `install --tools pi --dry-run` ended flag parsing at `pi` and ran a real install; a stray positional is now usage.
func TestInstallStrayPositionalIsUsage(t *testing.T) {
	got, _, stderr := hosttest.CaptureOutput(t, func() int { return RunInstall([]string{"--tools", "pi", "--dry-run"}) })
	if got != 2 || !strings.Contains(stderr, "--tools=pi") {
		t.Errorf("RunInstall = %d, stderr %q; want 2 naming --tools=pi", got, stderr)
	}
}

// Removed setup modes must be rejected before an install can run.
func TestInstallRejectsRemovedSetupModes(t *testing.T) {
	for _, mode := range []string{"adopt", "nuke", "workspace"} {
		for _, args := range [][]string{{"--setup", mode}, {"--dry-run", "--setup", mode}, {"--setup=" + mode}} {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return RunInstall(args) })
			if code != 2 || !strings.Contains(stderr, "setup") {
				t.Errorf("RunInstall(%v) = %d, stderr %q; want unknown flag", args, code, stderr)
			}
		}
	}
}
