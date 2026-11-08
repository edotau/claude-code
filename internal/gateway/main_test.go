package gateway

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/proxy"
)

func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test")
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	proxy.StubHolderCommForTest()
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}
