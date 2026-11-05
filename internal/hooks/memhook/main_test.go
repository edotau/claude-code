package memhook

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs every detached spawn and the live-process scan so no memhook test starts a process or reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &hosttest.MainTB{}
	StubSpawnForTest(tb, true)
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	cleanup()
	os.Exit(code)
}
