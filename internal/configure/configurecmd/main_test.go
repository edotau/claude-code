package configurecmd

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

// TestMain preserves the hermetic host used by these tests before they left cli.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test")
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	desktop.OnWSL = func() bool { return false } // the host's WSL-ness must not decide the desktop tests
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}
