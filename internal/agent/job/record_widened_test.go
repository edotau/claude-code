package job

import (
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
