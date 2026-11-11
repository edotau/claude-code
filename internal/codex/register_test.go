package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// codex exec has no ask-step to disable, so --autonomous is dropped and named in the envelope, not honored.
func TestCodexRunDropsAutonomous(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	agenttest.StubProxy(t, testRouterBase, testRouterSecret)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	// help prints no --json: codexHasJSON is cached per process and must not flip for other tests
	pathstest.FakeVendor(t, "codex", "cat >/dev/null\nif [ \"$2\" = --help ]; then exit 0; fi\necho ok\n")
	e, err := agent.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), e, agent.RunOptions{Prompt: "task", Provider: "toai", Tools: true, Autonomous: true}, wire.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Dropped, "--autonomous") {
		t.Errorf("Dropped = %q, want --autonomous", res.Dropped)
	}
}

// The effort clamp note survives agent.Run into the envelope, not only the engine's own Run.
func TestCodexAgentRunEnvelopeReportsEffortClamp(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	agenttest.StubProxy(t, testRouterBase, testRouterSecret)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	putCodexRecord(t, vault.AgentRecord{Effort: "max"})
	pathstest.FakeVendor(t, "codex", "cat >/dev/null\nif [ \"$2\" = --help ]; then exit 0; fi\necho ok\n")
	e, err := agent.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), e, agent.RunOptions{Prompt: "task", Provider: "toai"}, wire.Discard, nil)
	if err != nil || !slices.Contains(res.Emulated, "effort max→xhigh") {
		t.Errorf("Emulated = %q err=%v, want the max→xhigh clamp", res.Emulated, err)
	}
}
