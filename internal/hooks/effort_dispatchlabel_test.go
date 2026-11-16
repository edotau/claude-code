package hooks

import (
	"context"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// Source names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func TestLabelSource(t *testing.T) {
	for _, c := range []struct{ dispatch, label, session, want string }{
		{"opus", "opus", "s", "dispatch"}, {"inherit", "s", "s", "session"},
		{"", "s", "s", "session"}, {"", "sonnet", "s", "derived"}, {"", "", "", "derived"},
		{"opus", "databricks-gpt-6-1-sol", "sess-x", "derived"},
		{"opus", "sess-x", "sess-x", "session"},
		{"opus", "", "sess-x", "derived"},
		{"claude-opus-5", "databricks-claude-opus-5", "", "dispatch"},
		{"", "databricks-claude-opus-5", "claude-opus-5", "session"},
	} {
		if got := labelSource(c.dispatch, c.label, c.session); got != c.want {
			t.Errorf("labelSource(%q,%q,%q) = %q, want %q", c.dispatch, c.label, c.session, got, c.want)
		}
	}
}

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
	if changed := dispatchLabelsLine(in("databricks-claude-opus-5-5")); changed == "" {
		t.Fatalf("model change line = %q", changed)
	}
}

func TestDispatchLabelsLineListsEveryTierForUnpinnedTypes(t *testing.T) {
	useAgentsDir(t, "Explore")
	stubResolve(t, func(q agentpkg.ModelQuery) string { return "wire-" + q.Model })
	line := dispatchLabelsLine(core.ParseInputFor(strings.NewReader(`{"session_id":"s2","model":"sess-m"}`), core.AgentClaude))
	entry := line[strings.Index(line, "Explore="):]
	entry, _, _ = strings.Cut(entry, " ")
	for _, tier := range []string{"haiku", "sonnet", "opus"} {
		label, _, _ := dispatchLabel(context.Background(), LabelInput{Type: "Explore", Model: tier, SessionModel: "sess-m"})
		if label == "" || !strings.Contains(entry, label+":") {
			t.Errorf("tier %s: hook label %q missing from %q", tier, label, entry)
		}
	}
}
