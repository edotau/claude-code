package agent

import (
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

// A stray non-JSON line is logged and skipped: text before and after it both stream, with no error.
func TestParseOpenCodeStreamStrayLineKeepsStreamedText(t *testing.T) {
	stream := `{"type":"text","part":{"text":"before"}}` + "\n" +
		"warning: not json at all\n" +
		`{"type":"text","part":{"text":"after"}}` + "\n"
	rs := &recSink{}
	if _, err := parseOpenCodeStream(newBR(stream), rs, nil); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rs.types() != "text,text" || rs.evs[0].Text != "before" || rs.evs[1].Text != "after" {
		t.Fatalf("events = %s, want before then after", rs.types())
	}
}
