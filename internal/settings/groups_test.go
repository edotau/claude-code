package settings

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

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
