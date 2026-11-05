package copilot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
)

// CopilotJSON renders exactly the native copilot specs on events Copilot supports, each as one command.
func TestCopilotJSONRendersNativeSpecs(t *testing.T) {
	body, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Bash string `json:"bash"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for event, entries := range doc.Hooks {
		for _, e := range entries {
			got[event+"|"+strings.TrimSuffix(e.Bash, " || true")] = true
		}
	}
	want := map[string]bool{}
	for _, s := range settings.SpecsFor("copilot", "native") {
		if event := agentEvents[s.Event]; event != "" {
			want[event+"|"+s.Command()] = true
		}
	}
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("rendered %d commands, want %d: %v", len(got), len(want), got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %q", k)
		}
	}
}
