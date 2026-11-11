package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/textutil"
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
	EventProgress   = "progress" // a bare progress mark: counts as progress; EventLog and Stream drop it, JSONLSink keeps it
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

// Stamper numbers and times a sink's events; the caller holds the sink's lock.
type Stamper struct {
	seq   int64
	start time.Time
	host  string
}

func NewStamper() Stamper {
	host, _ := os.Hostname()
	return Stamper{start: Clock(), host: host}
}

// Stamp sets ev's schema, seq, t_ms and, when unset, host.
func (s *Stamper) Stamp(ev *Event) {
	s.seq++
	ev.Schema, ev.Seq, ev.TMs = EventSchema, s.seq, Clock().Sub(s.start).Milliseconds()
	if ev.Host == "" {
		ev.Host = s.host
	}
}

// JSONLSink writes one JSON object per line, stamping schema, seq, t_ms and the dispatch identity.
type JSONLSink struct {
	Stamper
	mu         sync.Mutex
	w          io.Writer
	dispatchID string
}

// NewJSONLSink is a JSONLSink over w for dispatchID.
func NewJSONLSink(w io.Writer, dispatchID string) *JSONLSink {
	return &JSONLSink{Stamper: NewStamper(), w: w, dispatchID: dispatchID}
}

// Emit stamps ev and writes it as one line.
func (s *JSONLSink) Emit(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Stamp(&ev)
	if ev.DispatchID == "" {
		ev.DispatchID = s.dispatchID
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

// SinkWriter is an io.Writer that emits its bytes as text events on s.
func SinkWriter(s Sink) io.Writer { return textWriter{s: s} }

// RunSink stamps the running agent's name and notes progress, so a retry never repeats streamed output or tool effects.
type RunSink struct {
	Sink
	Agent      string
	Progressed bool
}

func (r *RunSink) Emit(ev Event) error {
	if ev.Agent == "" {
		ev.Agent = r.Agent
	}
	r.Progressed = r.Progressed || ev.Progress() // before the sink: a failed emit still saw the event
	return r.Sink.Emit(ev)
}

// Progress is true for an event a replay could repeat: answer text, a tool call that may have had effects, or a mark.
func (ev Event) Progress() bool {
	switch ev.Type {
	case EventProgress:
		return true
	case EventText:
		return ev.Text != ""
	case EventToolUse, EventToolResult:
		return true
	}
	return false
}

// EmitError logs msg as an error line and emits it as an EventError, returning the sink's error.
func EmitError(lg StreamLog, sink Sink, msg string) error {
	lg.Emit("error", msg)
	return sink.Emit(Event{Type: EventError, Text: msg})
}

// ToolEvent is a tool_use event for the named tool.
func ToolEvent(name, id string) Event {
	return Event{Type: EventToolUse, Tool: &ToolRef{Name: name, ID: id}}
}

// ResultEvent is the closing event of a dispatch: the full envelope plus its headline fields.
func ResultEvent(env Envelope) Event {
	return Event{Type: EventResult, Agent: env.Agent, DispatchID: env.DispatchID, Status: env.Status,
		Usage: env.Usage, SessionID: env.SessionID, Envelope: &env}
}

// eventsTextCap bounds the text bytes one dispatch's event file keeps; later text events are dropped, the rest stay.
const eventsTextCap = 4 << 20

// EventLog writes <id>.events.jsonl (0600), redacted per line and text-capped; fail-open, Emit always returns nil.
type EventLog struct {
	mu      sync.Mutex
	f       *os.File
	bw      *bufio.Writer
	sink    *JSONLSink
	text    int
	pend    strings.Builder // text since the last newline, held back so a split secret is redacted whole
	pendSep bool
	pendEv  Event
}

// NewEventLog opens path for append; the returned log is a no-op when the file cannot be opened.
func NewEventLog(path, dispatchID string) *EventLog {
	l := &EventLog{}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return l
	}
	l.f, l.bw = f, bufio.NewWriter(f)
	l.sink = NewJSONLSink(l.bw, dispatchID)
	return l
}

// Emit buffers text per line and redacts before writing; every other event flushes the held text first.
func (l *EventLog) Emit(ev Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sink == nil {
		return nil
	}
	if ev.Type == EventProgress {
		return nil
	}
	if ev.Type != EventText {
		l.flushLocked()
		l.writeLocked(ev)
		return nil
	}
	if ev.Text == "" || l.text+len(ev.Text) > eventsTextCap {
		return nil // a chunk that would pass the cap is dropped whole, so no partial secret is stored
	}
	l.text += len(ev.Text)
	if ev.Sep {
		l.flushLocked() // a new turn starts a new line
	}
	if l.pend.Len() == 0 {
		l.pendEv, l.pendSep = ev, ev.Sep
	}
	l.pend.WriteString(ev.Text)
	l.drainLinesLocked()
	return nil
}

// drainLinesLocked writes the complete lines held in pend and keeps the unfinished tail.
func (l *EventLog) drainLinesLocked() {
	held := l.pend.String()
	i := strings.LastIndexByte(held, '\n')
	if i < 0 {
		if len(held) >= logLineCap {
			l.flushLocked() // a newline-free run is redacted and written as its own chunk, like LogSink
		}
		return
	}
	l.pend.Reset()
	l.pend.WriteString(held[i+1:])
	l.writeText(held[:i+1])
}

// flushLocked writes whatever text is held, newline or not.
func (l *EventLog) flushLocked() {
	if l.pend.Len() == 0 {
		return
	}
	held := l.pend.String()
	l.pend.Reset()
	l.writeText(held)
}

// writeText writes held as one text event carrying the pending event's identity and turn marker.
func (l *EventLog) writeText(held string) {
	ev := l.pendEv
	ev.Text, ev.Sep = held, l.pendSep
	l.pendSep = false
	l.writeLocked(ev)
}

// writeLocked redacts the event's free text, envelope answer and structured value, then writes it.
func (l *EventLog) writeLocked(ev Event) {
	if ev.Text != "" {
		ev.Text = textutil.RedactSecrets(ev.Text)
	}
	if ev.Envelope != nil && (ev.Envelope.Answer != "" || len(ev.Envelope.Structured) > 0) {
		env := *ev.Envelope
		env.Answer = textutil.RedactSecrets(env.Answer)
		if len(env.Structured) > 0 {
			env.Structured = redactJSON(env.Structured)
		}
		ev.Envelope = &env
	}
	_ = l.sink.Emit(ev) // fail-open
	_ = l.bw.Flush()    // bufio keeps one event's write atomic; flushing per event lets tail -f see it at once
}

// redactJSON redacts every string key and value in raw, numbers kept exact; undecodable raw is dropped, never kept.
func redactJSON(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	b, err := json.Marshal(redactValue(v))
	if err != nil {
		return nil
	}
	return b
}

// redactValue returns a copy of v with textutil.RedactSecrets applied to every string.
func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return textutil.RedactSecrets(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[textutil.RedactSecrets(k)] = redactValue(e)
		}
		return out
	}
	return v
}

// Close flushes held text and the buffer, then closes the file; it is safe to call twice.
func (l *EventLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	l.flushLocked()
	_ = l.bw.Flush()
	_ = l.f.Close()
	l.f, l.sink = nil, nil
}

// Answer collects the text a stream parser decoded; each chunk is recorded before it is emitted, so a failed emit keeps it.
type Answer struct {
	Join   string // between chunks in String: "" for deltas, a newline run for whole messages
	chunks []string
}

// Emit records text, then emits it as a text event; turns marks every chunk after the first as a new turn (Sep).
func (a *Answer) Emit(sink Sink, text string, turns bool) error {
	a.chunks = append(a.chunks, text)
	return sink.Emit(Event{Type: EventText, Text: text, Sep: turns && len(a.chunks) > 1})
}

// Empty is true until a chunk has been emitted.
func (a *Answer) Empty() bool { return len(a.chunks) == 0 }

// String is the chunks joined by Join and trimmed.
func (a *Answer) String() string { return strings.TrimSpace(strings.Join(a.chunks, a.Join)) }

// Last is the final chunk, trimmed: the answer of an agent whose last message is the reply.
func (a *Answer) Last() string {
	if a.Empty() {
		return ""
	}
	return strings.TrimSpace(a.chunks[len(a.chunks)-1])
}
