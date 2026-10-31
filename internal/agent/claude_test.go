package agent

import (
	"errors"
	"io"
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
	res, err := parseClaudeStream(newBR(claudeFixture), NewTextSink(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello\nworld" {
		t.Errorf("stream = %q (subagent text must be skipped)", out.String())
	}
	if res.Answer != "Hello\nworld" || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Usage != (Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 4, CostUSD: 0.01}) {
		t.Errorf("res = %+v", res)
	}
}

func TestParseClaudeStreamErrors(t *testing.T) {
	isErr := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429 rate_limit_error"}` + "\n"
	_, err := parseClaudeStream(newBR(isErr), NewTextSink(&strings.Builder{}))
	if e := Classify("claude", err, false); e == nil || e.Kind != KindRateLimit {
		t.Errorf("is_error result = %v", err)
	}
	if _, err := parseClaudeStream(newBR(`{"type":"system","subtype":"init"}`), NewTextSink(&strings.Builder{})); err == nil {
		t.Error("a stream without a result event must fail")
	}
	if _, err := parseClaudeStream(newBR(`{"type":`), NewTextSink(&strings.Builder{})); err == nil {
		t.Error("truncated JSON must fail")
	}
}

func TestParseClaudeStreamResultApplication(t *testing.T) {
	stream := `{"type":"system","session_id":"init-id","model":"init-model"}
	{"type":"assistant","message":{"model":"answer-model","content":[{"type":"text","text":""},{"type":"thinking","text":"hidden"},{"type":"text","text":"one"},{"type":"text","text":"two"}]}}
	{"type":"assistant","parent_tool_use_id":"tool-1","message":{"model":"subagent-model","content":[{"type":"text","text":"hidden"}]}}
	{"type":"result","session_id":"result-id","is_error":true,"subtype":"error_during_execution","result":"partial result","usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":2,"cache_read_input_tokens":4}}`
	var out strings.Builder
	res, err := parseClaudeStream(newBR(stream), NewTextSink(&out))
	if err == nil || err.Error() != "claude error_during_execution: partial result" {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "one\ntwo" || res.Answer != "partial result" || res.SessionID != "result-id" || res.Model != "answer-model" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
	if res.Usage != (Usage{InputTokens: 7, OutputTokens: 3, CacheCreationTokens: 2, CacheReadTokens: 4}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestParseClaudeStreamWriteError(t *testing.T) {
	res, err := parseClaudeStream(newBR(claudeFixture), NewTextSink(failingStreamWriter{}))
	if !errors.Is(err, io.ErrClosedPipe) || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Answer != "" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

type newlineRejectWriter struct {
	writes []string
	err    error
}

func (w *newlineRejectWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	if string(p) == "\n" {
		return 0, w.err
	}
	return len(p), nil
}

func TestParseClaudeStreamNewlineError(t *testing.T) {
	for name, stream := range map[string]string{
		"blocks": `{"type":"assistant","message":{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]}}
{"type":"result","result":"one\ntwo\nthree"}`,
		"events": `{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"three"}]}}
{"type":"result","result":"one\ntwo\nthree"}`,
	} {
		t.Run(name, func(t *testing.T) {
			injected := errors.New("newline rejected")
			w := &newlineRejectWriter{err: injected}
			_, err := parseClaudeStream(newBR(stream), NewTextSink(w))
			if !errors.Is(err, injected) {
				t.Fatalf("err = %v, want injected newline error", err)
			}
			if len(w.writes) != 2 || w.writes[0] != "one" || w.writes[1] != "\n" {
				t.Fatalf("writes = %q, want only first text and rejected newline", w.writes)
			}
		})
	}
}
