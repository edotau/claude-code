package core

import (
	"encoding/json"
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

// A gemini payload in the Claude shape (no toolCall) keeps its tool_name and tool_input.
func TestParseInputGeminiClaudeShape(t *testing.T) {
	in := ParseInputFor(strings.NewReader(`{"tool_name":"run_command","tool_input":{"command":"ls"}}`), AgentGemini)
	if in.CanonicalTool() != "Bash" || in.ToolCommand() != "ls" || in.Invalid != "" {
		t.Errorf("tool %q command %q invalid %q; want Bash, ls, valid", in.CanonicalTool(), in.ToolCommand(), in.Invalid)
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

// opencode is its own agent from the env or the payload key its host writes; an unknown name still means Claude.
func TestParseInputRecognisesOpencode(t *testing.T) {
	cases := []struct{ name, env, payload, want string }{
		{"env", AgentOpencode, `{"session_id":"s","hook_event_name":"Stop"}`, AgentOpencode},
		{"payload key", "", `{"session_id":"s","hook_event_name":"Stop","harness_agent":"opencode"}`, AgentOpencode},
		{"payload key over codex env", AgentCodex, `{"hook_event_name":"Stop","harness_agent":"opencode"}`, AgentOpencode},
		{"unknown env", "kilo", `{"session_id":"s","hook_event_name":"Stop"}`, AgentClaude},
		{"unknown payload key", "", `{"hook_event_name":"Stop","harness_agent":"kilo"}`, AgentClaude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_HOOK_AGENT", tc.env)
			if got := ParseInput(strings.NewReader(tc.payload)).Agent(); got != tc.want {
				t.Fatalf("Agent() = %q, want %q", got, tc.want)
			}
		})
	}
}

// agy's own fields fill the session, event and loop-guard fields, and a tool call carries Claude's tool name.
func TestParseInputGeminiNativeFields(t *testing.T) {
	stop := ParseInputFor(strings.NewReader(`{"conversationId":"c1","hookEventName":"Stop","cwd":"/w","continuationActive":true}`), AgentGemini)
	if stop.SessionID != "c1" || stop.HookEventName != "Stop" || stop.CWD != "/w" || !stop.StopHookActive {
		t.Errorf("stop payload: %+v", stop)
	}
	call := ParseInputFor(strings.NewReader(`{"toolCall":{"name":"write_file","args":{"Path":"/w/N.md","Content":"x"}},"workspacePaths":["/w"]}`), AgentGemini)
	if call.ToolName != "Write" || call.Nested("tool_input", "file_path") != "/w/N.md" || call.CWD != "/w" {
		t.Errorf("write_file: tool %q input %v cwd %q", call.ToolName, call.ToolInput(), call.CWD)
	}
}

func BenchmarkParseInputFor(b *testing.B) {
	payload := `{"session_id":"s1","cwd":"/tmp/x","tool_name":"Bash","tool_input":{"command":"git status --short"},"hook_event_name":"PreToolUse"}`
	b.ReportAllocs()
	for b.Loop() {
		ParseInputFor(strings.NewReader(payload), AgentClaude)
	}
}

// EditedPaths reads every agent's edit shape; a shell command edits nothing and a malformed header is skipped.
func TestEditedPathsEveryAgentShape(t *testing.T) {
	const patch = "*** Begin Patch\r\n*** Add File: a.md \r\n+x\r\n+y\r\n*** Update File: b.go\r\n@@\r\n+z\r\n" +
		"*** Delete File: c.go\r\n*** Move to: d.go\r\n*** Add File:\r\n*** End Patch"
	want := "add:a.md update:b.go delete:c.go move:d.go"
	for _, tc := range []struct{ name, doc, want string }{
		{"claude Write", `{"tool_name":"Write","tool_input":{"file_path":"/r/a.go"}}`, "update:/r/a.go"},
		{"gemini write_file", `{"tool_name":"write_file","tool_input":{"file_path":"/r/a.go"}}`, "update:/r/a.go"},
		{"copilot create", `{"toolName":"create","toolArgs":{"path":"/r/a.go"}}`, "update:/r/a.go"},
		{"copilot filePath", `{"toolName":"edit","toolArgs":{"filePath":"/r/a.go"}}`, "update:/r/a.go"},
		{"opencode edit", `{"tool_name":"edit","tool_input":{"filePath":"/r/a.go"}}`, "update:/r/a.go"},
		{"opencode apply_patch", `{"tool_name":"apply_patch","tool_input":{"patchText":` + jsonString(patch) + `}}`, want},
		{"codex command", `{"tool_name":"apply_patch","tool_input":{"command":` + jsonString(patch) + `}}`, want},
		{"codex input", `{"tool_name":"apply_patch","tool_input":{"input":` + jsonString(patch) + `}}`, want},
		{"empty patch", `{"tool_name":"apply_patch","tool_input":{"command":""}}`, ""},
		{"bash is no edit", `{"tool_name":"Bash","tool_input":{"command":` + jsonString(patch) + `}}`, ""},
	} {
		in := ParseInputFor(strings.NewReader(tc.doc), strings.SplitN(tc.name, " ", 2)[0])
		var got []string
		for _, e := range in.EditedPaths() {
			got = append(got, string(e.Kind)+":"+e.Path)
		}
		if g := strings.Join(got, " "); g != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, g, tc.want)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
