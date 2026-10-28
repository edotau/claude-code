package launch

import (
	"context"
	"strings"
	"testing"
)

const dbxRow = `"dbx":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"w",` +
	`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5"}}`

func TestPresetRowNonClaudeModelGoesThroughRouter(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+dbxRow+`}}}`)
	p, err := Claude(context.Background(), Options{Provider: "dbx", Model: "databricks-gpt-5-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := p.Set["ANTHROPIC_BASE_URL"]; !strings.HasPrefix(base, "http://127.0.0.1:4000") {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the loopback router", base)
	}
}

func TestPresetRowClaudeModelStaysDirect(t *testing.T) {
	hermetic(t, `{`+noModels+`,"providers":{"databricks":{`+dbxRow+`}}}`)
	p, err := Claude(context.Background(), Options{Provider: "dbx", Model: "databricks-claude-opus-5-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base := p.Set["ANTHROPIC_BASE_URL"]; strings.HasPrefix(base, "http://127.0.0.1") {
		t.Errorf("ANTHROPIC_BASE_URL = %q, Claude model must go direct", base)
	}
}
