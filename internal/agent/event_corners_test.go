package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// recSink records every event.
type recSink struct{ evs []Event }

func (r *recSink) Emit(ev Event) error { r.evs = append(r.evs, ev); return nil }

func (r *recSink) types() string {
	var ts []string
	for _, e := range r.evs {
		ts = append(ts, e.Type)
	}
	return strings.Join(ts, ",")
}

// TextSink over a multi-turn stream writes exactly the plain bytes the pre-event streaming wrote.
func TestTextSinkMatchesPlainStreamBytes(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"one"},{"type":"tool_use","id":"t","name":"Bash"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"one\ntwo"}` + "\n"
	var out strings.Builder
	if _, err := parseClaudeStream(strings.NewReader(stream), NewTextSink(&out)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "one\ntwo" {
		t.Fatalf("plain output = %q, want %q", out.String(), "one\ntwo")
	}
}

// A subagent turn (parent_tool_use_id) emits no text event and no tool event; only the top-level turn does.
func TestParseClaudeStreamSubagentTurnEmitsNoEvents(t *testing.T) {
	stream := `{"type":"assistant","parent_tool_use_id":"tu1","message":{"content":[{"type":"text","text":"SUB"},{"type":"tool_use","id":"x","name":"Bash"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"top"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"top"}` + "\n"
	rs := &recSink{}
	if _, err := parseClaudeStream(strings.NewReader(stream), rs); err != nil {
		t.Fatal(err)
	}
	if len(rs.evs) != 1 || rs.evs[0].Type != EventText || rs.evs[0].Text != "top" {
		b, _ := json.Marshal(rs.evs)
		t.Fatalf("events = %s, want one text event \"top\"", b)
	}
}

// A stray non-JSON line ends the opencode stream with a decode error (json.Decoder cannot resync, same as before
// events); text already streamed stays emitted and the partial answer is not lost.
func TestParseOpenCodeStreamStrayLineKeepsStreamedText(t *testing.T) {
	stream := `{"type":"text","part":{"text":"before"}}` + "\n" +
		"warning: not json at all\n" +
		`{"type":"text","part":{"text":"after"}}` + "\n"
	rs := &recSink{}
	_, err := parseOpenCodeStream(strings.NewReader(stream), rs, nil)
	if err == nil || !strings.Contains(err.Error(), "decode opencode json") {
		t.Fatalf("err = %v, want a decode error", err)
	}
	if rs.types() != "text" || rs.evs[0].Text != "before" {
		t.Fatalf("events = %s, want the one streamed text before the stray line", rs.types())
	}
}
