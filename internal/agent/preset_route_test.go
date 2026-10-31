package agent

import (
	"context"
	"strings"
	"testing"
)

const presetDbxRow = `"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"w",` +
	`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}`

func TestPresetRowNonClaudeModelGoesThroughRouter(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+presetDbxRow+`}}}`)
	p, err := Claude(context.Background(), LaunchOptions{Provider: "dbx", Model: "databricks-kimi-k3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := p.Set["ANTHROPIC_BASE_URL"]; !strings.HasPrefix(base, "http://127.0.0.1:4000") {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the loopback router", base)
	}
}

// D14: a Responses-only model has no translate path for Claude Code; the launch refuses and points at codex/copilot.
func TestPresetRowResponsesOnlyModelRefusedForClaudeCode(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+presetDbxRow+`}}}`)
	_, err := Claude(context.Background(), LaunchOptions{Provider: "dbx", Model: "databricks-gpt-5-5"}, nil)
	if err == nil {
		t.Fatal("Claude Code launched on a Responses-only model")
	}
	for _, want := range []string{"databricks-gpt-5-5", "Responses-only", "codex", "copilot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, missing %q", err, want)
		}
	}
}

func TestPresetRowClaudeModelStaysDirect(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+presetDbxRow+`}}}`)
	p, err := Claude(context.Background(), LaunchOptions{Provider: "dbx", Model: "databricks-claude-opus-5-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := p.Set["ANTHROPIC_BASE_URL"]; strings.HasPrefix(base, "http://127.0.0.1") {
		t.Errorf("ANTHROPIC_BASE_URL = %q, Claude model must go direct", base)
	}
}
