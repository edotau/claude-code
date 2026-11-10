package wthook

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs the live-process scans so no wthook test reads the machine's agents or worktree users.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	PeerAgents = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
