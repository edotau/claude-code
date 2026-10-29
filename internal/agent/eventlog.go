package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

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
	if ev.Type != EventText {
		l.flushLocked()
		l.writeLocked(ev)
		if ev.Type == EventResult || ev.Type == EventError {
			_ = l.bw.Flush()
		}
		return nil
	}
	if ev.Text == "" || l.text > eventsTextCap {
		return nil
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

// writeLocked redacts the event's free text and envelope answer, then writes it.
func (l *EventLog) writeLocked(ev Event) {
	if ev.Text != "" {
		ev.Text = shapes.Redact(ev.Text)
	}
	if ev.Envelope != nil && ev.Envelope.Answer != "" {
		env := *ev.Envelope
		env.Answer = shapes.Redact(env.Answer)
		ev.Envelope = &env
	}
	_ = l.sink.Emit(ev) // fail-open
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

// DispatchOpts tunes Dispatch for the caller's stdout contract.
type DispatchOpts struct {
	Job      bool         // a --job child: stdout is the log, so out is dropped and the log keeps events + stderr only
	AnswerTo io.Writer    // written the final answer once the log is flushed (a job's `<agent> result` reads it)
	Notify   func(string) // receives non-fatal bookkeeping failures (the record not saving)
}

// Dispatch runs one recorded dispatch (log, event file, start, run, Finish/Save, result): one pair per id, not per leg.
func Dispatch(ctx context.Context, rec *Record, agentName string, out Sink, o DispatchOpts,
	run func(context.Context, Sink) (Result, error)) (Result, error) {
	logSink := NewLogSink(rec.LogPath())
	defer logSink.Close() // idempotent: covers a panic in Run
	ctx = WithLog(ctx, logSink)
	if o.Job {
		ctx, out = WithLogEvents(ctx, logSink), Discard
	}
	events := NewEventLog(rec.EventsPath(), rec.ID)
	defer events.Close()
	sink := Multi(out, events)
	begin := time.Now()
	if err := sink.Emit(Event{Type: EventStart, Agent: agentName, DispatchID: rec.ID, Status: "running"}); err != nil {
		return Result{Agent: agentName, Status: StatusError}, err
	}
	res, err := run(ctx, sink)
	logSink.Close() // flush before the record leaves running, so tail -f never misses the last line
	if o.AnswerTo != nil && res.Answer != "" {
		_, _ = io.WriteString(o.AnswerTo, res.Answer)
	}
	rec.Finish(res, err, time.Since(begin))
	if serr := rec.Save(); serr != nil && o.Notify != nil {
		o.Notify(fmt.Sprintf("dispatch %s record not saved, it stays at running: %v", rec.ID, serr))
	}
	if rerr := sink.Emit(ResultEvent(NewEnvelope(res, err, rec.ID))); rerr != nil && err == nil {
		err = rerr
	}
	return res, err
}
