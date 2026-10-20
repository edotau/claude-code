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
	if !IsHarness("$HOME/.claude/bin/claude-code hook bash-guard") || IsHarness("$HOME/.claude/bin/claude-code-other x") || IsHarness("orca hook") {
		t.Error("IsHarness")
	}
}

// TestPlanHooksRegistered pins plan-template and plan-mirror's events/matchers/gate so a later edit to the
// registry can't silently drop or misroute either hook.
func TestPlanHooksRegistered(t *testing.T) {
	byName := map[string]Spec{}
	for _, s := range Registry {
		byName[s.Name] = s
	}
	tmpl, ok := byName["plan-template"]
	if !ok || tmpl.Event != EventPreToolUse || tmpl.Matcher != "Write" || !tmpl.Gate {
		t.Fatalf("plan-template = %+v, ok=%v", tmpl, ok)
	}
	mirror, ok := byName["plan-mirror"]
	if !ok || mirror.Event != EventPostToolUse || mirror.Matcher != "Write|Edit" || mirror.Gate {
		t.Fatalf("plan-mirror = %+v, ok=%v", mirror, ok)
	}
	b, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block map[string][]Group
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range block[EventPreToolUse] {
		if g.Matcher != "Write" {
			continue
		}
		for _, h := range g.Hooks {
			if h.Command == tmpl.Command() {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("rendered settings missing plan-template under PreToolUse/Write: %s", b)
	}
}
