package agent

import (
	"strings"
	"testing"
)

// permissionDeniedEvent is sampled from the real claude CLI binary (2.1.304, strings -a dump of the
// installed `claude` bundle): a "system","permission_denied" event carries "message" as a plain
// string, not the {model, content[]} object every assistant/user event uses. It is the real shape
// behind the production failure recorded in state/harness/dispatch (decode stream-json: json: cannot
// unmarshal string into Go struct field claudeEvent.message).
const permissionDeniedEvent = `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_1","agent_id":"","decision_reason_type":"rule","decision_reason_code":"deny","decision_reason":"blocked by settings","message":"Bash is not allowed in this session","session_id":"sess-2"}`

// TestParseClaudeStreamStringMessage is the RED fixture: one odd event (a string "message") must
// never abort decoding of the rest of a real stream (a normal assistant turn plus the result event).
func TestParseClaudeStreamStringMessage(t *testing.T) {
	stream := permissionDeniedEvent + "\n" +
		`{"type":"assistant","session_id":"sess-2","message":{"model":"claude-opus-5","content":[{"type":"text","text":"done"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-2"}` + "\n"
	var out strings.Builder
	res, err := parseClaudeStream(strings.NewReader(stream), &out)
	if err != nil {
		t.Fatalf("a string message event must not fail the leg: %v", err)
	}
	if out.String() != "done" || res.Answer != "done" || res.SessionID != "sess-2" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
}

// TestParseClaudeStreamErrorEventText surfaces an "error"-type event's string message as the returned error.
func TestParseClaudeStreamErrorEventText(t *testing.T) {
	stream := `{"type":"error","message":"fatal: connection reset","session_id":"sess-3"}` + "\n"
	_, err := parseClaudeStream(strings.NewReader(stream), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "fatal: connection reset") {
		t.Fatalf("err = %v, want the error event's message text", err)
	}
}
