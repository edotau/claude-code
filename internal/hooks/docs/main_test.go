package docs

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs the live-process scan so no docs test reads the real machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	code := m.Run()
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
