package agents

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook/memhooktest"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs the live-process scans, detached spawns, the usage cache and the route pick so no test reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &hosttest.MainTB{}
	memhooktest.StubSpawn(tb, true)
	ClaudePressure = func() (float64, string, bool) { return 0, "", false } // the real usage cache never steers a test
	RouteHint = func(string) (string, string) { return "codex", "gpt-6.1-sol" }
	wthook.PeerAgents = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	gittest.Cleanup()
	cleanup()
	os.Exit(code)
}

// codexHooks are the codex-* modules by name, with the hookspec event each renders for.
var codexHooks = map[string]struct {
	event string
	run   core.Runner
}{
	"codex-agent-route":    {settings.EventPreToolUse, CodexAgentRoute},
	"codex-routing-start":  {settings.EventSessionStart, CodexRoutingStart},
	"codex-routing-result": {settings.EventPostToolUse, CodexRoutingResult},
	"codex-subagent-start": {settings.EventSubagentStart, CodexSubagentStart},
}

// runCodex runs one codex module on stdin and renders its Result as the vendor wire output, as the hook verb does.
func runCodex(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	h := codexHooks[name]
	in := core.ParseInput(stdin)
	return core.Emit(in.Agent(), h.event, h.run(in), stdout, stderr)
}

// runEvent runs the dispatch runner a payload's event is declared on.
func runEvent(t *testing.T, event, payload string) string {
	t.Helper()
	runners := map[string]core.Runner{settings.EventPreToolUse: Effort, settings.EventPostToolUseFailure: Release,
		settings.EventSubagentStop: Stop}
	in := core.ParseInput(strings.NewReader(payload))
	var out, errb bytes.Buffer
	core.Emit(in.Agent(), event, runners[event](in), &out, &errb)
	return out.String()
}
