package cli

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	cleanup := hosttest.Setup()
	desktop.OnWSL = func() bool { return false } // the host's WSL-ness must not decide the desktop tests
	code := m.Run()
	cleanup()
	os.Exit(code)
}
