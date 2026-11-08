package memorycmd

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// TestMain preserves the hermetic host used by these tests before they left cli.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test")
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}

func helpPageHasCommand(page helptext.Page, name string) bool {
	for _, row := range page.Commands {
		words := strings.Fields(row[0])
		if len(words) > 0 && words[0] == name {
			return true
		}
	}
	return false
}
