package cli

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test") // the moved batch/envelope tests reach the proxy
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	gateway.HolderComm = func(int) string { return "claude-code" } // router-state tests record this test binary's pid as the proxy
	desktop.OnWSL = func() bool { return false }                   // the host's WSL-ness must not decide the desktop tests
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}

// cliScratch isolates CLAUDE_CONFIG_DIR, every credential env var, and PATH (no real copilot/gh/etc.
// binary reachable) so provider/model commands never touch real machine state.
func cliScratch(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no external credential helpers or other CLIs
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", providers.EnvSessionProfile} {
		t.Setenv(k, "")
	}
}

func renderHelpPage(page helptext.Page) string {
	var out bytes.Buffer
	page.Fprint(&out)
	return out.String()
}

// termWidth is a line's display width with tabs expanded to 8-column stops.
func termWidth(l string) int {
	w := 0
	for _, r := range l {
		if r == '\t' {
			w += 8 - w%8
		} else {
			w++
		}
	}
	return w
}

// A command printing past the pipe buffer must not deadlock: the audit hung 5m on a large settings render.
func TestCaptureOutputDrainsPastThePipeBuffer(t *testing.T) {
	want := 256 << 10
	code, stdout, _ := hosttest.CaptureOutput(t, func() int {
		if _, err := os.Stdout.Write(make([]byte, want)); err != nil {
			return 1
		}
		return 0
	})
	if code != 0 || len(stdout) != want {
		t.Fatalf("code %d, captured %d bytes, want %d", code, len(stdout), want)
	}
}
