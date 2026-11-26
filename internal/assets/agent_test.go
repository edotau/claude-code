package assets

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentTemplatesTakeValuesVerbatim(t *testing.T) {
	hostile := "{{.Task}} <<<X \"q\" 'a'\n"
	if got := PackPrompt(hostile, "", ""); !strings.Contains(got, "<task>\n"+hostile+"\n</task>") {
		t.Fatalf("value was altered: %q", got)
	}
}

func TestInventoryListing(t *testing.T) {
	rows := Inventory()
	if len(rows) != len(inventory) {
		t.Fatalf("%d rows, want %d", len(rows), len(inventory))
	}
	byPath := map[string]PromptRow{}
	for _, r := range rows {
		byPath[r.Path] = r
	}
	if r := byPath["hooks/misc.tmpl"]; r.Family != "hooks" || !slices.Equal(r.Accessors, []string{"HookDiverged"}) || r.Size == 0 ||
		!slices.Equal(r.Fields, []string{"Ahead", "Behind", "Branch", "Rewritten", "Up"}) {
		t.Errorf("misc row: %+v", r)
	}
	if r := byPath["hooks/claim-lane-hint.txt"]; len(r.Fields) != 0 {
		t.Errorf("plain text row has fields: %+v", r)
	}
}
