package settings

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSettingsJSON(t *testing.T) {
	b, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block map[string][]MatcherGroup
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	if len(block[EventStop]) != 1 || len(block[EventStop][0].Hooks) != 1 || len(block[EventSessionStart]) != 1 || len(block[EventSessionEnd]) != 1 || len(block[EventUserPromptSubmit]) != 1 || block[EventPreToolUse][0].Matcher != "Bash" {
		t.Errorf("block %s", b)
	}
	if !IsHarness("$HOME/.claude/bin/claude-code hook PreToolUse --safety-scan") || IsHarness("$HOME/.claude/bin/claude-code-other x") || IsHarness("orca hook") {
		t.Error("IsHarness")
	}
}

func TestIsHarness(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    bool
	}{
		{HookBin + " hook x", true},
		{"rtk hook claude", true}, // the stale entry `rtk init -g` writes
		{"/opt/bin/rtk hook claude", true},
		{"rtk hook codex", false},
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

func TestPlanHooksRegistered(t *testing.T) {
	byName := map[string]EventSpec{}
	docsCount := 0
	for _, s := range Specs() {
		byName[s.Name] = s
		if s.Name == "docs-placement" {
			docsCount++
		}
	}
	if _, ok := byName["plan-template"]; ok {
		t.Fatal("retired plan-template hook remains registered")
	}
	docs, ok := byName["docs-placement"]
	if !ok || docsCount != 1 || docs.Group != "docs" || docs.Event != EventPreToolUse || docs.Matcher != "Write|Edit" || !docs.Blocks {
		t.Fatalf("docs-placement = %+v, ok=%v, count=%d", docs, ok, docsCount)
	}
	if g, _ := GroupOf("docs-placement"); g.Gate {
		t.Fatal("docs group is a gate group; docs-placement must run inside the dispatcher")
	}
	mirror, ok := byName["docs-plan-mirror"]
	if !ok || mirror.Event != EventPostToolUse || mirror.Matcher != "Write|Edit" || mirror.Blocks {
		t.Fatalf("docs-plan-mirror = %+v, ok=%v", mirror, ok)
	}
	b, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block map[string][]MatcherGroup
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	found := 0
	retired := EventSpec{Name: "plan-template", Event: EventPreToolUse}
	for event, groups := range block {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command == retired.Command() {
					t.Fatalf("rendered settings contain retired plan-template: %s", b)
				}
				if h.Command == DispatcherCommand(EventPreToolUse) {
					found++
					if event != EventPreToolUse || !strings.Contains(g.Matcher, "Write|Edit") {
						t.Errorf("PreToolUse dispatcher carries matcher %q, want it to cover docs-placement's Write|Edit", g.Matcher)
					}
				}
			}
		}
	}
	if found != 1 {
		t.Errorf("rendered settings contain %d PreToolUse dispatcher entries, want 1: %s", found, b)
	}
}

// A merged dispatcher entry keeps the spinner text only when exactly one module on the event sets one.
func TestEventsDispatcherStatusMessage(t *testing.T) {
	old := Registry
	defer func() { Registry = old }()
	status := func(a, b string) string {
		Registry = []Group{{Name: "x", Events: []EventSpec{
			{Name: "x-a", Event: EventSessionStart, Budget: 5 * sec, StatusMessage: a},
			{Name: "x-b", Event: EventSessionStart, Budget: 5 * sec, StatusMessage: b},
		}}}
		return Events()[EventSessionStart][0].Hooks[0].StatusMessage
	}
	if got := status("Loading", ""); got != "Loading" {
		t.Fatalf("one status: %q", got)
	}
	if got := status("Loading", "Other"); got != "" {
		t.Fatalf("two statuses: %q", got)
	}
}

// One dispatcher entry per event, plus own entries only for Gate or Async handlers.
func TestGroupRenderOneEntryPerEvent(t *testing.T) {
	b, err := SettingsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var block map[string][]MatcherGroup
	if err := json.Unmarshal(b, &block); err != nil {
		t.Fatal(err)
	}
	own := map[string]int{}
	for _, s := range Specs() {
		g, _ := GroupOf(s.Name)
		if !g.AgentOnly && (g.Gate || s.Async) {
			own[s.Event]++
		}
	}
	for ev, entries := range block {
		dispatchers := 0
		for _, e := range entries {
			for _, h := range e.Hooks {
				if h.Command == DispatcherCommand(ev) {
					dispatchers++
				}
			}
		}
		if dispatchers != 1 {
			t.Errorf("%s: %d dispatcher entries", ev, dispatchers)
		}
		if len(entries) < 1+own[ev] {
			t.Errorf("%s: %d entries, want at least %d", ev, len(entries), 1+own[ev])
		}
	}
	if n := len(block[EventPostToolUse]); n != 1 {
		t.Errorf("PostToolUse: %d entries, want 1 dispatcher (access-trace, the only async handler, is gone)", n)
	}
	if n := len(block[EventPreToolUse]); n != 2 {
		t.Errorf("PreToolUse: %d entries, want 2 (safety-scan alone, then the dispatcher)", n)
	}
	if !strings.Contains(string(b), `"`+DispatcherCommand("PreToolUse")+`"`) || !strings.Contains(string(b), "hook PreToolUse --safety-scan") {
		t.Errorf("safety must render separately and the dispatcher must skip it: %s", b)
	}
}

func TestGroupNoEventLostVsGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/settings.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ Hooks map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	got := Events()
	for ev := range golden.Hooks {
		if len(got[ev]) == 0 {
			t.Errorf("event %s lost", ev)
		}
	}
}
