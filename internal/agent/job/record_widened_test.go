package job

import (
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// The dispatch record logs what the request grants beyond the agent's defaults (D3), not what the defaults grant.
func TestNewRecordLogsWidening(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"codex":{"permission":{"defaults":{"allow":["Read"]}}}}}`)
	p, err := agent.ParsePermissions("", []string{"Read", "Bash(touch:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", agent.RunOptions{Prompt: "p", Permissions: p}, time.Now())
	if want := []string{"allow Bash(touch:*)"}; !slices.Equal(rec.Widened, want) {
		t.Errorf("Widened = %q, want %q", rec.Widened, want)
	}
}

// A fallback run is judged against the agent that answered; an unreadable fallback default keeps the original list.
func TestFinishRecomputesWideningForFallbackAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Put(t, vault.Overlay, "agents",
		`{"agents":{"codex":{"permission":{"defaults":{"allow":["Read"]}}},"gemini":{"permission":{"defaults":{"deny":["Bash("]}}}}}`)
	p, err := agent.ParsePermissions("", []string{"Read"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecord("codex", agent.RunOptions{Prompt: "p", Permissions: p}, time.Now())
	rec.Finish(wire.Result{Agent: "copilot", Fallback: "codex 429"}, nil, time.Second)
	if want := []string{"allow Read"}; !slices.Equal(rec.Widened, want) {
		t.Errorf("Widened after fallback = %q, want %q", rec.Widened, want)
	}
	rec2 := NewRecord("codex", agent.RunOptions{Prompt: "p", Permissions: p}, time.Now())
	rec2.Finish(wire.Result{Agent: "gemini", Fallback: "codex 429"}, nil, time.Second)
	if len(rec2.Widened) != 0 {
		t.Errorf("bad fallback default overwrote Widened: %q", rec2.Widened)
	}
}
