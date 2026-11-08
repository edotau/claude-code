package client

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
)

// TestMain runs in the hermetic host (hosttest) with the vendor roster registered and the proxy stubbed.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	agent.StubProxyForTest("http://127.0.0.1:9999", "test-router-secret")
	os.Exit(hosttest.Main(m))
}
