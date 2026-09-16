package agent

import (
	"strings"
	"testing"
)

const claudeFixture = `{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5","tools":["Bash"]}
{"type":"system","subtype":"hook_started","session_id":"sess-1"}
{"type":"assistant","session_id":"sess-1","parent_tool_use_id":null,"message":{"model":"claude-opus-5","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Hello"}]}}
{"type":"assistant","session_id":"sess-1","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"subagent chatter"}]}}
{"type":"user","session_id":"sess-1","message":{"content":[{"type":"tool_result","content":"x"}]}}
{"type":"stream_event","session_id":"sess-1","event":{}}
{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"world"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Hello\nworld","session_id":"sess-1","num_turns":2,"total_cost_usd":0.01,"usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":4}}
`

func TestParseClaudeStream(t *testing.T) {
	var out strings.Builder
	res, err := parseClaudeStream(strings.NewReader(claudeFixture), &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello\nworld" {
		t.Errorf("stream = %q (subagent text must be skipped)", out.String())
	}
	if res.Answer != "Hello\nworld" || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Usage != (Usage{100, 4}) {
		t.Errorf("res = %+v", res)
	}
}

func TestParseClaudeStreamErrors(t *testing.T) {
	isErr := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429 rate_limit_error"}` + "\n"
	_, err := parseClaudeStream(strings.NewReader(isErr), &strings.Builder{})
	if e := Classify("claude", err, false); e == nil || e.Kind != KindRateLimit {
		t.Errorf("is_error result = %v", err)
	}
	if _, err := parseClaudeStream(strings.NewReader(`{"type":"system","subtype":"init"}`), &strings.Builder{}); err == nil {
		t.Error("a stream without a result event must fail")
	}
	if _, err := parseClaudeStream(strings.NewReader(`{"type":`), &strings.Builder{}); err == nil {
		t.Error("truncated JSON must fail")
	}
}
