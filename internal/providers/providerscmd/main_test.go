package providerscmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude" // register the roster agents the switch tests move
	_ "github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
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

func captureClosedStdout(t *testing.T, run func() int) (int, string) {
	t.Helper()
	closed, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		stdout := os.Stdout
		os.Stdout = closed
		defer func() { os.Stdout = stdout }()
		return run()
	})
	return code, stderr
}

// useTLSTransport routes http.DefaultClient through srv's TLS client and closes srv at cleanup.
func useTLSTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
}
