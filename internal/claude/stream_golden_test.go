package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/edotau/claude-code/internal/agent"
)

type goldenSink func(agent.Event)

func (f goldenSink) Emit(ev agent.Event) error { f(ev); return nil }

// TestParseClaudeStreamGolden runs a scrubbed real `claude -p --output-format stream-json` capture through the parser.
func TestParseClaudeStreamGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var tools []string
	sink := goldenSink(func(ev agent.Event) {
		switch ev.Type {
		case agent.EventText:
			text.WriteString(ev.Text)
		case agent.EventToolUse:
			tools = append(tools, ev.Tool.Name)
		}
	})
	res, err := parseClaudeStream(newBR(string(raw)), sink) // includes plain-string message lines
	if err != nil {
		t.Fatalf("golden stream: %v", err)
	}
	const answer = "Hi! Here's what `ls` shows in `/home/user/project`."
	assertGoldenClaudeResult(t, res, text.String(), answer, tools)
	if deltas := goldenStreamDeltas(t, raw); deltas != answer {
		t.Errorf("stream_event text deltas = %q", deltas)
	}
}

func assertGoldenClaudeResult(t *testing.T, res agent.Result, text, answer string, tools []string) {
	t.Helper()
	if text != answer || res.Answer != answer {
		t.Errorf("text = %q, answer = %q", text, res.Answer)
	}
	if len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("tool events = %q, want one Bash", tools)
	}
	if res.SessionID != "uuid_01" || res.Model != "claude-opus-5-5" || res.Usage.CostUSD != 0.359989 || res.Usage.OutputTokens != 470 {
		t.Errorf("res = %+v", res)
	}
}

func goldenStreamDeltas(t *testing.T, raw []byte) string {
	t.Helper()
	var deltas strings.Builder
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var inner struct {
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &inner); err != nil || inner.Type != "stream_event" {
			continue
		}
		var se anthropic.MessageStreamEventUnion
		if err := json.Unmarshal(inner.Event, &se); err != nil {
			t.Fatal(err)
		}
		if d, ok := se.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if td, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				deltas.WriteString(td.Text)
			}
		}
	}
	return deltas.String()
}
