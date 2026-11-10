package worktree

import (
	"github.com/edotau/claude-code/internal/hosttest"

	"os"
	"testing"
)

// TestMain pins every state/config path to a temp dir so no test can touch the real ~/.claude, and git startup is trimmed by hosttest.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	StubProcessScanForTest()
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}
