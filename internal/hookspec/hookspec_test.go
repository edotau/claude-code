package hookspec

import (
	"encoding/json"
	"strings"
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
	if len(block[EventStop]) != 1 || len(block[EventStop][0].Hooks) != 1 || len(block[EventSessionStart]) != 1 || len(block[EventSessionEnd]) != 1 || len(block[EventUserPromptSubmit]) != 1 || block[EventPreToolUse][0].Matcher != "Bash" {
		t.Errorf("block %s", b)
	}
	if !IsHarness("$HOME/.claude/bin/claude-code hook bash-guard") || IsHarness("$HOME/.claude/bin/claude-code-other x") || IsHarness("orca hook") {
		t.Error("IsHarness")
	}
}

func TestIsHarness(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    bool
	}{
		{HookBin + " hook x", true},
		{"HARNESS_HOOK_AGENT=codex " + HookBin + " hook x", true},
		{"HARNESS_HOOK_AGENT=vendor_2 " + HookBin + " hook x", true},
		{"HARNESS_HOOK_VENDOR=codex " + HookBin + " hook x", true}, // the pre-rename marker
		{"FOO=1 " + HookBin + " hook x", false},
		{"HARNESS_HOOK_AGENT=codex orca hook", false},
		{HookBin + "-other hook x", false},
		{"HARNESS_HOOK_AGENT=codex " + HookBin + "-other hook x", false},
		{"HARNESS_HOOK_AGENT= " + HookBin + " hook x", false},
		{"HARNESS_HOOK_AGENT=codex; " + HookBin + " hook x", false},
		{"HARNESS_HOOK_AGENT=codex FOO=1 " + HookBin + " hook x", false},
	} {
		t.Run(tt.command, func(t *testing.T) {
			if got := IsHarness(tt.command); got != tt.want {
				t.Errorf("IsHarness(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestCodexJSONVendorMarker(t *testing.T) {
	b, err := CodexJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, groups := range block.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				count++
				if !strings.HasPrefix(hook.Command, "HARNESS_HOOK_AGENT=codex $HOME/.claude/bin/claude-code hook ") {
					t.Errorf("Codex command missing vendor marker: %q", hook.Command)
				}
				if !IsHarness(hook.Command) {
					t.Errorf("Codex command not recognized as harness: %q", hook.Command)
				}
			}
		}
	}
	if count == 0 {
		t.Fatal("no Codex commands rendered")
	}
}

func TestPlanHooksRegistered(t *testing.T) {
	byName := map[string]Spec{}
	docsCount := 0
	for _, s := range Registry {
		byName[s.Name] = s
		if s.Name == "docs-placement" {
			docsCount++
		}
	}
	if _, ok := byName["plan-template"]; ok {
		t.Fatal("retired plan-template hook remains registered")
	}
	docs, ok := byName["docs-placement"]
	if !ok || docsCount != 1 || docs.Event != EventPreToolUse || docs.Matcher != "Write|Edit" || !docs.Gate {
		t.Fatalf("docs-placement = %+v, ok=%v, count=%d", docs, ok, docsCount)
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
	found := 0
	retired := Spec{Name: "plan-template"}
	for event, groups := range block {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command == retired.Command() {
					t.Fatalf("rendered settings contain retired plan-template: %s", b)
				}
				if h.Command == docs.Command() {
					found++
					if event != EventPreToolUse || g.Matcher != "Write|Edit" {
						t.Errorf("docs-placement rendered under %s/%s", event, g.Matcher)
					}
				}
			}
		}
	}
	if found != 1 {
		t.Errorf("rendered settings contain %d docs-placement hooks, want 1: %s", found, b)
	}
}
