package agent

import (
	"slices"
	"strings"
	"testing"
	"time"
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
	res, err := parseClaudeStream(strings.NewReader(stream), NewTextSink(&out))
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
	_, err := parseClaudeStream(strings.NewReader(stream), NewTextSink(&strings.Builder{}))
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
	old := clock
	clock = func() time.Time { d := ticks[i]; i++; return base.Add(d) }
	defer func() { clock = old }()
	var out strings.Builder
	var lines []string
	res, err := parseClaudeStream(strings.NewReader(stream), NewTextSink(&out), func(k, x string) { lines = append(lines, k+": "+x) })
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

func TestApplyCapsDropsIsolation(t *testing.T) {
	req := Request{NoPersist: true, SafeMode: true, DebugFile: "/x"}
	if got, d, _ := applyCaps(Caps{}, req); got.NoPersist || got.SafeMode || got.DebugFile != "" ||
		!slices.Equal(d, []string{"--no-persist", "--safe-mode", "--debug-file=/x"}) {
		t.Fatalf("got %+v dropped %v", got, d)
	}
	if got, d, _ := applyCaps(Caps{NoPersist: Native, SafeMode: Native, DebugFile: Native}, req); !got.NoPersist || !got.SafeMode || got.DebugFile != "/x" || d != nil {
		t.Fatalf("isolation caps must keep all three: %+v %v", got, d)
	}
}
