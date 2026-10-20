package agent

import (
	"strings"
	"testing"
)

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
	res, err := parseAgyStream(strings.NewReader(truncated), &out)
	if err == nil || err.Error() != "agy: stream ended without a result event" {
		t.Fatalf("err = %v, want \"agy: stream ended without a result event\"", err)
	}
	// The step_update text that did arrive before truncation is still surfaced, not thrown away.
	if res.Answer != "pong" || out.String() != "pong" {
		t.Errorf("res.Answer = %q, streamed %q, want %q (the pre-truncation delta)", res.Answer, out.String(), "pong")
	}
}
