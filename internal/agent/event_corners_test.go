package agent

import (
	"strings"
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

