package hookspec

import (
	"encoding/json"
	"testing"
)

func TestSettingsJSON(t *testing.T) {
	b, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block map[string][]Group
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	if len(block[EventStop]) != 1 || len(block[EventStop][0].Hooks) != 3 || len(block[EventSessionStart]) != 1 || len(block[EventSessionEnd]) != 1 || len(block[EventUserPromptSubmit]) != 1 || block[EventPreToolUse][0].Matcher != "Bash" {
		t.Errorf("block %s", b)
	}
	if !IsHarness("$HOME/.claude/bin/claude-code hook safety") || IsHarness("$HOME/.claude/bin/claude-code-other x") || IsHarness("orca hook") {
		t.Error("IsHarness")
	}
}
