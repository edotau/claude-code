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
