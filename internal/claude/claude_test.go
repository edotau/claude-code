package claude

import (
	"bufio"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
	res, err := parseClaudeStream(newBR(claudeFixture), agent.NewTextSink(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello\nworld" {
		t.Errorf("stream = %q (subagent text must be skipped)", out.String())
	}
	if res.Answer != "Hello\nworld" || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Usage != (agent.Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 4, CostUSD: 0.01}) {
		t.Errorf("res = %+v", res)
	}
}

func TestParseClaudeStreamErrors(t *testing.T) {
	isErr := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429 rate_limit_error"}` + "\n"
	_, err := parseClaudeStream(newBR(isErr), agent.NewTextSink(&strings.Builder{}))
	if e := agent.Classify("claude", err, false); e == nil || e.Kind != agent.KindRateLimit {
		t.Errorf("is_error result = %v", err)
	}
	if _, err := parseClaudeStream(newBR(`{"type":"system","subtype":"init"}`), agent.NewTextSink(&strings.Builder{})); err == nil {
		t.Error("a stream without a result event must fail")
	}
	if _, err := parseClaudeStream(newBR(`{"type":`), agent.NewTextSink(&strings.Builder{})); err == nil {
		t.Error("truncated JSON must fail")
	}
}

func TestParseClaudeStreamResultApplication(t *testing.T) {
	stream := `{"type":"system","session_id":"init-id","model":"init-model"}
	{"type":"assistant","message":{"model":"answer-model","content":[{"type":"text","text":""},{"type":"thinking","text":"hidden"},{"type":"text","text":"one"},{"type":"text","text":"two"}]}}
	{"type":"assistant","parent_tool_use_id":"tool-1","message":{"model":"subagent-model","content":[{"type":"text","text":"hidden"}]}}
	{"type":"result","session_id":"result-id","is_error":true,"subtype":"error_during_execution","result":"partial result","usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":2,"cache_read_input_tokens":4}}`
	var out strings.Builder
	res, err := parseClaudeStream(newBR(stream), agent.NewTextSink(&out))
	if err == nil || err.Error() != "claude error_during_execution: partial result" {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "one\ntwo" || res.Answer != "partial result" || res.SessionID != "result-id" || res.Model != "answer-model" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
	if res.Usage != (agent.Usage{InputTokens: 7, OutputTokens: 3, CacheCreationTokens: 2, CacheReadTokens: 4}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestParseClaudeStreamWriteError(t *testing.T) {
	res, err := parseClaudeStream(newBR(claudeFixture), agent.NewTextSink(failingStreamWriter{}))
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
			_, err := parseClaudeStream(newBR(stream), agent.NewTextSink(w))
			if !errors.Is(err, injected) {
				t.Fatalf("err = %v, want injected newline error", err)
			}
			if len(w.writes) != 2 || w.writes[0] != "one" || w.writes[1] != "\n" {
				t.Fatalf("writes = %q, want only first text and rejected newline", w.writes)
			}
		})
	}
}

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
	res, err := parseClaudeStream(newBR(stream), agent.NewTextSink(&out))
	if err != nil {
		t.Fatalf("a string message event must not fail the dispatch: %v", err)
	}
	if out.String() != "done" || res.Answer != "done" || res.SessionID != "sess-2" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
}

// TestParseClaudeStreamErrorEventText surfaces an "error"-type event's string message as the returned error.
func TestParseClaudeStreamErrorEventText(t *testing.T) {
	stream := `{"type":"error","message":"fatal: connection reset","session_id":"sess-3"}` + "\n"
	_, err := parseClaudeStream(newBR(stream), agent.NewTextSink(&strings.Builder{}))
	if err == nil || !strings.Contains(err.Error(), "fatal: connection reset") {
		t.Fatalf("err = %v, want the error event's message text", err)
	}
}

// TestParseClaudeStreamTransport: stream_event timing and api_retry counts land in Transport; answer text is unchanged.
func TestParseClaudeStreamTransport(t *testing.T) {
	stream := `{"type":"stream_event","event":{"type":"message_start"}}` + "\n" +
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":529,"error":"overloaded"}` + "\n" +
		`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1200,"error_status":null,"error":"timeout"}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n" +
		`{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"hi"}` + "\n"
	base := time.Unix(1000, 0)
	ticks := []time.Duration{0, 150 * time.Millisecond, 950 * time.Millisecond} // start, event 1, event 2
	i := 0
	old := agent.Clock
	agent.Clock = func() time.Time { d := ticks[i]; i++; return base.Add(d) }
	defer func() { agent.Clock = old }()
	var out strings.Builder
	var lines []string
	res, err := parseClaudeStream(newBR(stream), agent.NewTextSink(&out), func(k, x string) { lines = append(lines, k+": "+x) })
	if err != nil || res.Answer != "hi" || out.String() != "hi" {
		t.Fatalf("res=%+v out=%q err=%v", res, out.String(), err)
	}
	tr := res.Transport
	if tr == nil || tr.Retries != 2 || !slices.Equal(tr.RetryStatuses, []int{529, 0}) || tr.FirstEventMs != 150 || tr.MaxGapMs != 800 {
		t.Fatalf("transport = %+v", tr)
	}
	if len(lines) < 1 || lines[0] != "retry: attempt 1/10 status=529 retry_in=500ms" {
		t.Fatalf("log lines = %v", lines)
	}
}

func newBR(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

type failingStreamWriter struct{}

func (failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Reconstructed from dispatch 20261101T214810.199-claude's log (raw bytes are not kept): a background subagent
// makes the session emit a result, resume, emit another, then close on an empty result.
func TestParseClaudeStreamKeepsLastNonEmptyResult(t *testing.T) {
	stream := `{"type":"assistant","session_id":"s","message":{"content":[{"type":"tool_use","name":"Agent","id":"toolu_1"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"The codex agent is still running.","session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"result":"The dispatch ran and the agent replied PONG-7.","session_id":"s","total_cost_usd":0.2154}
{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s","total_cost_usd":0.2154}
`
	res, err := parseClaudeStream(newBR(stream), agent.NewTextSink(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if want := "The dispatch ran and the agent replied PONG-7."; res.Answer != want {
		t.Errorf("Answer = %q, want %q", res.Answer, want)
	}
}
