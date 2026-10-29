package agent

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseAgyStreamResultApplication(t *testing.T) {
	const init = `{"event":"init","conversation_id":"init-id"}` + "\n"
	const delta = `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":" partial "}}` + "\n"
	cases := []struct {
		name, stream, answer, output, session, err string
		usage                                      Usage
	}{
		{
			name: "response fallback and result metadata",
			stream: init + `{"event":"result","result":{"status":"SUCCESS","conversation_id":"result-id",
				"response":" answer \n","usage":{"input_tokens":7,"output_tokens":3,"thinking_tokens":2,"cache_read_tokens":4}}}`,
			answer: "answer", output: " answer \n", session: "result-id",
			usage: Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 4},
		},
		{
			name:   "deltas win and init session survives",
			stream: init + delta + `{"event":"result","result":{"status":"SUCCESS","response":"duplicate"}}`,
			answer: "partial", output: " partial ", session: "init-id",
		},
		{
			name: "failure retains partial answer and usage",
			stream: init + delta + `{"event":"result","result":{"status":"ERROR","response":"not emitted",
				"error":"upstream failed","usage":{"input_tokens":7,"thinking_tokens":2}}}`,
			answer: "partial", output: " partial ", session: "init-id", err: "upstream failed",
			usage: Usage{InputTokens: 7, OutputTokens: 2},
		},
		{
			name: "status error fallback", stream: `{"event":"result","result":{"status":"CANCELLED","response":"not emitted"}}`,
			err: "agy run CANCELLED",
		},
		{
			name: "missing status error fallback", stream: `{"event":"result","result":{}}`,
			err: "agy run failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			res, err := parseAgyStream(strings.NewReader(tc.stream), NewTextSink(&out), nil)
			if (err == nil) != (tc.err == "") || err != nil && err.Error() != tc.err {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if res.Answer != tc.answer || out.String() != tc.output || res.SessionID != tc.session || res.Usage != tc.usage {
				t.Fatalf("res = %+v, output = %q", res, out.String())
			}
		})
	}
}

// agyArgv maps Autonomous onto "--mode auto"; --dangerously-skip-permissions rides on Tools alone, so
// an autonomous-but-tool-less run must never carry it (agy must never auto-grant write access it wasn't
// asked for).
func TestAgyArgvAutonomous(t *testing.T) {
	cases := []struct {
		name               string
		tools, autonomous  bool
		wantMode, wantSkip bool
	}{
		{"neither", false, false, false, false},
		{"tools only", true, false, false, true},
		{"autonomous only", false, true, true, false},
		{"both", true, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(AgyRequest{Model: "m", Tools: c.tools, Autonomous: c.autonomous})
			hasMode := strings.Contains(strings.Join(argv, " "), "--mode auto")
			hasSkip := slices.Contains(argv, "--dangerously-skip-permissions")
			if hasMode != c.wantMode || hasSkip != c.wantSkip {
				t.Errorf("argv = %q, mode=%v skip=%v, want mode=%v skip=%v", argv, hasMode, hasSkip, c.wantMode, c.wantSkip)
			}
		})
	}
}

// AgyRequest.SessionID rides as "--conversation <id>"; an empty SessionID must never emit the flag
// (agy has no bare --conversation value to resume against).
func TestAgyArgvConversation(t *testing.T) {
	cases := []struct{ name, session string }{{"unset", ""}, {"set", "conv-123"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(AgyRequest{Model: "m", SessionID: c.session})
			idx := slices.Index(argv, "--conversation")
			if has := idx >= 0; has != (c.session != "") {
				t.Fatalf("argv = %q, --conversation present = %v, want %v", argv, has, c.session != "")
			}
			if c.session != "" && argv[idx+1] != c.session {
				t.Errorf("argv = %q, --conversation value = %q, want %q", argv, argv[idx+1], c.session)
			}
		})
	}
}

type failingStreamWriter struct{}

func (failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestParseAgyStreamWriteErrors(t *testing.T) {
	for _, stream := range []string{
		agyOK,
		`{"event":"result","result":{"status":"SUCCESS","response":"fallback"}}`,
	} {
		if _, err := parseAgyStream(strings.NewReader(stream), NewTextSink(failingStreamWriter{}), nil); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("err = %v, want closed pipe", err)
		}
	}
}

// truncateBeforeResult drops every line at-and-after the "result" event from a stream-json fixture,
// simulating a connection that dies mid-run (the agy binary itself was not reachable in this sandbox
// to capture a fresh truncated stream live, so this derives the truncation from the agyOK fixture
// above, itself captured live from `agy --output-format stream-json` 1.4.2).
func truncateBeforeResult(t *testing.T, stream string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stream, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.Contains(l, `"event":"result"`) {
			break
		}
		kept = append(kept, l)
	}
	if len(kept) == len(lines) {
		t.Fatalf("fixture has no result event to truncate before: %q", stream)
	}
	return strings.Join(kept, "\n") + "\n"
}

// A stream that EOFs before any result event is a truncated connection, not a quiet success: agy.go
// must surface it rather than returning a zero-Usage "success" with whatever text happened to stream.
func TestParseAgyStream_TruncatedNoResult(t *testing.T) {
	truncated := truncateBeforeResult(t, agyOK)
	var out strings.Builder
	res, err := parseAgyStream(strings.NewReader(truncated), NewTextSink(&out), nil)
	if err == nil || err.Error() != "agy: stream ended without a result event" {
		t.Fatalf("err = %v, want \"agy: stream ended without a result event\"", err)
	}
	// The step_update text that did arrive before truncation is still surfaced, not thrown away.
	if res.Answer != "pong" || out.String() != "pong" {
		t.Errorf("res.Answer = %q, streamed %q, want %q (the pre-truncation delta)", res.Answer, out.String(), "pong")
	}
}

func TestAgyArgvDebugFile(t *testing.T) {
	if slices.Contains(agyArgv(AgyRequest{Model: "m"}), "--log-file") {
		t.Fatal("--log-file without DebugFile")
	}
	argv := agyArgv(AgyRequest{Model: "m", DebugFile: "/tmp/l"})
	if i := slices.Index(argv, "--log-file"); i < 0 || argv[i+1] != "/tmp/l" {
		t.Fatalf("argv = %v", argv)
	}
}
