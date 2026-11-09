package core

import (
	"strings"
	"testing"
)

func TestParseInputGeminiNullFailsOpen(t *testing.T) {
	t.Setenv("HARNESS_HOOK_AGENT", AgentGemini)
	in := ParseInput(strings.NewReader("null"))
	if in.Agent() != AgentGemini || string(in.Raw) != "null" {
		t.Fatalf("Agent=%q Raw=%q", in.Agent(), in.Raw)
	}
	if in.ToolName != "" || in.CWD != "" || len(in.ToolInput()) != 0 {
		t.Fatalf("null input populated tool fields: %+v", in)
	}
}

func TestParseInputStewardFields(t *testing.T) {
	in := ParseInput(strings.NewReader(`{"background_tasks":[{"id":"a"},{"id":"b"}],"trigger":"auto"}`))
	if in.BackgroundTasks != 2 || in.Trigger != "auto" {
		t.Fatalf("BackgroundTasks=%d Trigger=%q", in.BackgroundTasks, in.Trigger)
	}
}
