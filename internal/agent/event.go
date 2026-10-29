package agent

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// EventSchema versions the `ask --format stream-json` event line.
const EventSchema = 1

// Event.Type values.
const (
	EventStart      = "start"
	EventText       = "text"
	EventToolUse    = "tool_use"
	EventToolResult = "tool_result"
	EventRetry      = "retry"
	EventResult     = "result"
	EventError      = "error"
)

// ToolRef names the tool a tool_use or tool_result event is about.
type ToolRef struct {
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Event is one agent-normalized NDJSON line; a result event carries the full envelope.
type Event struct {
	Schema     int       `json:"schema"`
	DispatchID string    `json:"dispatch_id,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Host       string    `json:"host,omitempty"`
	Seq        int64     `json:"seq"`
	TMs        int64     `json:"t_ms"` // milliseconds since the sink was made
	Type       string    `json:"type"`
	Text       string    `json:"text,omitempty"`
	Sep        bool      `json:"sep,omitempty"` // this text starts a new turn: text sinks put a newline before it
	Tool       *ToolRef  `json:"tool,omitempty"`
	Status     string    `json:"status,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Envelope   *Envelope `json:"envelope,omitempty"`
}

// Sink receives a run's events. Emit is synchronous; its error is the consumer's write failure (a closed pipe).
type Sink interface{ Emit(Event) error }

// Discard is the sink that drops every event.
var Discard Sink = discardSink{}

type discardSink struct{}

func (discardSink) Emit(Event) error { return nil }

// TextSink writes each text event's text to w: the plain streaming output, byte for byte.
type TextSink struct{ w io.Writer }

// NewTextSink is a TextSink over w.
func NewTextSink(w io.Writer) *TextSink { return &TextSink{w: w} }

// Emit writes a text event's text, a newline first when it starts a new turn; other event types are ignored.
func (s *TextSink) Emit(ev Event) error {
	if ev.Type != EventText {
		return nil
	}
	if ev.Sep {
		if _, err := io.WriteString(s.w, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(s.w, ev.Text)
	return err
}

// JSONLSink writes one JSON object per line, stamping schema, seq, t_ms and the dispatch identity.
type JSONLSink struct {
	mu         sync.Mutex
	w          io.Writer
	dispatchID string
	host       string
	start      time.Time
	seq        int64
}

// NewJSONLSink is a JSONLSink over w for dispatchID.
func NewJSONLSink(w io.Writer, dispatchID string) *JSONLSink {
	host, _ := os.Hostname()
	return &JSONLSink{w: w, dispatchID: dispatchID, host: host, start: clock()}
}

// Emit stamps ev and writes it as one line.
func (s *JSONLSink) Emit(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.Schema, ev.Seq, ev.TMs = EventSchema, s.seq, clock().Sub(s.start).Milliseconds()
	if ev.DispatchID == "" {
		ev.DispatchID = s.dispatchID
	}
	if ev.Host == "" {
		ev.Host = s.host
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.w.Write(append(b, '\n'))
	return err
}

type multiSink []Sink

// Emit delivers ev to every sink, then returns the first error; one failing consumer never starves the others.
func (m multiSink) Emit(ev Event) error {
	var first error
	for _, s := range m {
		if err := s.Emit(ev); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Multi fans each event out to every non-nil sink in order.
func Multi(sinks ...Sink) Sink {
	var out multiSink
	for _, s := range sinks {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

// textWriter turns each Write into a text event, for children whose stdout is plain text.
type textWriter struct{ s Sink }

func (w textWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.s.Emit(Event{Type: EventText, Text: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// sinkWriter is an io.Writer that emits its bytes as text events on s.
func sinkWriter(s Sink) io.Writer { return textWriter{s: s} }

// runSink stamps the running agent's name and counts text bytes, so a retry never repeats streamed output.
type runSink struct {
	Sink
	agent string
	n     int64
}

func (r *runSink) Emit(ev Event) error {
	if ev.Agent == "" {
		ev.Agent = r.agent
	}
	if ev.Type == EventText {
		r.n += int64(len(ev.Text))
	}
	return r.Sink.Emit(ev)
}

// toolEvent is a tool_use event for the named tool.
func toolEvent(name, id string) Event {
	return Event{Type: EventToolUse, Tool: &ToolRef{Name: name, ID: id}}
}

// ResultEvent is the closing event of a dispatch: the full envelope plus its headline fields.
func ResultEvent(env Envelope) Event {
	return Event{Type: EventResult, Agent: env.Agent, DispatchID: env.DispatchID, Status: env.Status,
		Usage: env.Usage, SessionID: env.SessionID, Envelope: &env}
}
