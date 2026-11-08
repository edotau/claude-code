package codex

import (
	"bufio"
	"github.com/edotau/claude-code/internal/agent/wire"
	"strings"
	"testing"
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
