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
