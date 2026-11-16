package agents

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestDispatchLabelsLineRefreshesOnModelChange(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	in := func(model string) *core.Input {
		return core.ParseInputFor(strings.NewReader(`{"session_id":"s1","model":"`+model+`"}`), core.AgentClaude)
	}
	first := dispatchLabelsLine(in("databricks-claude-sonnet-5-5"))
	if !strings.HasPrefix(first, "Dispatch labels (description prefix): ") || !strings.Contains(first, "Explore=") {
		t.Fatalf("first line = %q", first)
	}
	if again := dispatchLabelsLine(in("databricks-claude-sonnet-5-5")); again != "" {
		t.Fatalf("unchanged model re-emitted %q", again)
	}
	if changed := dispatchLabelsLine(in("databricks-claude-opus-5-5")); changed == "" || changed == first {
		t.Fatalf("model change line = %q", changed)
	}
}
