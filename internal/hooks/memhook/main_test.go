package memhook

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// mainTB lets TestMain call the testing.TB stubs: Cleanup is deferred until the whole run ends.
type mainTB struct {
	testing.TB
	cleanups []func()
}

func (m *mainTB) Helper()          {}
func (m *mainTB) Cleanup(f func()) { m.cleanups = append(m.cleanups, f) }

// TestMain stubs every detached spawn and the live-process scan so no memhook test starts a process or reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &mainTB{}
	StubSpawnForTest(tb, true)
	code := m.Run()
	for _, f := range tb.cleanups {
		f()
	}
	cleanup()
	os.Exit(code)
}
