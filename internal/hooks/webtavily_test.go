package hooks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestWebTavilyOffDatabricksLeavesResult(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/p/no-such-profile")
	in := core.ParseInputFor(strings.NewReader(`{"tool_name":"WebFetch","tool_input":{"url":"https://example.com","prompt":"p"},"tool_response":{"result":"x"}}`), "claude")
	if r := WebTavily(in); r.UpdatedToolOutput != nil {
		t.Errorf("got a replacement off Databricks: %v", r.UpdatedToolOutput)
	}
}
