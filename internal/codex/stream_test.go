package codex

import (
	"bufio"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// A completed answer survives a later typed-decode failure; a garbage line before it stays tolerated.
func TestParseCodexStreamDecodeErrorKeepsAnswer(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"th-1"}
not json
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":123}`
	res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "done" || res.SessionID != "th-1" {
		t.Fatalf("res = %+v, err = %v; want the decode error with answer and session kept", res, err)
	}
}

// cached_input_tokens above input_tokens must not push the summed input usage negative.
func TestParseCodexStreamCachedInputNeverNegative(t *testing.T) {
	stream := `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":5}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":30,"output_tokens":2}}`
	res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	want := wire.Usage{InputTokens: 60, CacheReadTokens: 70, OutputTokens: 7}
	if err != nil || res.Usage != want {
		t.Fatalf("usage = %+v, err = %v; want %+v", res.Usage, err, want)
	}
}

func TestParseCodexStreamTopLevelErrors(t *testing.T) {
	const retry = `{"type":"error","will_retry":true,"message":"temporary outage"}`
	const done = "\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"42"}}` + "\n" + `{"type":"turn.completed"}`
	cases := []struct{ name, events, answer, failure string }{
		{"recovered", retry + done, "42", ""},
		{"fatal", `{"type":"error","message":"permanent failure"}` + done, "42", "permanent failure"},
		{"truncated", retry, "", "temporary outage"},
		{"turn failed", `{"type":"turn.failed","will_retry":true,"error":{"message":"turn failure"}}` + done, "42", "turn failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := `{"type":"thread.started","thread_id":"probe"}` + "\n" + tc.events
			res, err := parseCodexStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
			failure := ""
			if err != nil {
				failure = err.Error()
			}
			t.Logf("answer=%q error=%v", res.Answer, err)
			if res.Answer != tc.answer || failure != tc.failure {
				t.Fatalf("answer=%q error=%s; want answer=%q error=%s", res.Answer, failure, tc.answer, tc.failure)
			}
		})
	}
}
