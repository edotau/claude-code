package core

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// A payload that cannot be read or parsed is Invalid and keeps the explicit agent; empty or field-less JSON is not.
func TestParseInputForMarksUnparseablePayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       io.Reader
		invalid bool
	}{
		{"syntax error", strings.NewReader(`{not json`), true},
		{"truncated", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls -l`), true},
		{"read error", iotest.ErrReader(errors.New("pipe closed")), true},
		{"null", strings.NewReader(` null`), true},
		{"valid without fields", strings.NewReader(`{}`), false},
		{"empty", strings.NewReader(""), false},
		{"blank", strings.NewReader(" \n"), false},
	} {
		in := ParseInputFor(tc.r, AgentGemini)
		if (in.Invalid != "") != tc.invalid {
			t.Errorf("%s: Invalid=%q, want invalid=%t", tc.name, in.Invalid, tc.invalid)
		}
		if tc.invalid && in.Agent() != AgentGemini {
			t.Errorf("%s: Agent=%q, want the explicit %q so a block renders for it", tc.name, in.Agent(), AgentGemini)
		}
	}
}

// Tool arguments present in a shape the host never sends are Invalid; every legitimate per-host shape stays valid.
func TestParseInputForMarksMisshapenToolArgs(t *testing.T) {
	for _, tc := range []struct {
		name, agent, body string
		invalid           bool
	}{
		{"copilot broken toolArgs", "", `{"sessionId":"s","toolName":"Bash","toolArgs":"{"}`, true},
		{"copilot toolArgs array", AgentCopilot, `{"toolName":"bash","toolArgs":["rm"]}`, true},
		{"gemini string args", AgentGemini, `{"toolCall":{"name":"run_command","args":"rm x"}}`, true},
		{"gemini string toolCall", AgentGemini, `{"toolCall":"run_command"}`, true},
		{"claude string tool_input", AgentClaude, `{"tool_name":"Bash","tool_input":"oops"}`, true},
		{"codex array tool_input", AgentCodex, `{"tool_name":"Bash","tool_input":["ls"]}`, true},
		{"stop without tool", AgentClaude, `{"hook_event_name":"Stop","stop_hook_active":false}`, false},
		{"copilot string toolArgs", AgentCopilot, `{"toolName":"bash","toolArgs":"{\"command\":\"ls\"}"}`, false},
		{"copilot object toolArgs", "", `{"sessionId":"s","toolName":"bash","toolArgs":{"command":"ls"}}`, false},
		{"copilot empty toolArgs", AgentCopilot, `{"toolName":"view","toolArgs":""}`, false},
		{"copilot prompt without toolArgs", AgentCopilot, `{"sessionId":"s","prompt":"hi"}`, false},
		{"gemini args", AgentGemini, `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`, false},
		{"gemini empty args", AgentGemini, `{"toolCall":{"name":"list_dir","args":{}}}`, false},
		{"gemini no toolCall", AgentGemini, `{"workspacePaths":["/w"]}`, false},
		{"claude tool_input", AgentClaude, `{"tool_name":"Bash","tool_input":{"command":"ls"}}`, false},
		{"codex null tool_input", AgentCodex, `{"tool_name":"spawn_agent","tool_input":null}`, false},
	} {
		if in := ParseInputFor(strings.NewReader(tc.body), tc.agent); (in.Invalid != "") != tc.invalid {
			t.Errorf("%s: Invalid=%q, want invalid=%t", tc.name, in.Invalid, tc.invalid)
		}
	}
}

func TestParseInputGeminiNullPopulatesNothing(t *testing.T) {
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

// The explicit hook agent names the settings spec and the roster use are the ones Input recognises.
func TestParseInputForRecognisesRosterNames(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "gemini"} {
		if got := ParseInputFor(strings.NewReader(`{}`), name).Agent(); got != name {
			t.Errorf("agent %q parsed as %q", name, got)
		}
	}
}
