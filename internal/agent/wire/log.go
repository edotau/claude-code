package wire

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// logLineCap bounds one buffered log line; a longer run is redacted and written as its own chunk.
const logLineCap = 256 << 10

// LogSink tees a dispatch's child output, redacted line by line, into its log file (0600, append).
// Safe for the stdout and stderr streams of every fallback dispatch to share; a write failure never reaches the run.
type LogSink struct {
	path    string
	mu      sync.Mutex
	f       *os.File
	failed  bool
	streams []*logLineWriter
}

// NewLogSink makes a sink for path; the file is created when the first child starts.
func NewLogSink(path string) *LogSink { return &LogSink{path: path} }

type logSinkKey struct{}

// logCarrier is the ctx value: the sink plus whether raw stdout tees are suppressed (job runs).
type logCarrier struct {
	sink  *LogSink
	noTee bool
}

// WithLog carries sink in ctx so every agent child started under it tees into the log.
func WithLog(ctx context.Context, sink *LogSink) context.Context {
	return context.WithValue(ctx, logSinkKey{}, logCarrier{sink: sink})
}

// WithLogEvents is WithLog for a job run, whose caller writes the final answer itself: event lines and child
// stderr reach the log, raw child stdout never does.
func WithLogEvents(ctx context.Context, sink *LogSink) context.Context {
	return context.WithValue(ctx, logSinkKey{}, logCarrier{sink: sink, noTee: true})
}

// logFrom is the sink for raw stdout tees; nil when absent or a job carrier.
func logFrom(ctx context.Context) *LogSink {
	if c, _ := ctx.Value(logSinkKey{}).(logCarrier); !c.noTee {
		return c.sink
	}
	return nil
}

// stderrLogFrom is the sink for child-stderr tees and event lines, job carriers included.
func StderrLogFrom(ctx context.Context) *LogSink {
	c, _ := ctx.Value(logSinkKey{}).(logCarrier)
	return c.sink
}

// TeeWriter returns w that also feeds ctx's log, or w itself when the dispatch has none.
func TeeWriter(ctx context.Context, w io.Writer) io.Writer {
	if s := logFrom(ctx); s != nil {
		return io.MultiWriter(w, s.Stream())
	}
	return w
}

// StreamLog writes one formatted event line to the dispatch log; a nil StreamLog is a no-op.
type StreamLog func(kind, text string)

// eventTextCap bounds the text of one `▸ kind: text` log line, in runes.
const eventTextCap = 300

// EventLogFor binds ctx's sink; nil when the dispatch has none.
func EventLogFor(ctx context.Context) StreamLog {
	if StderrLogFrom(ctx) == nil {
		return nil
	}
	return func(kind, text string) { LogEvent(ctx, kind, text) }
}

func (l StreamLog) Emit(kind, text string) {
	if l != nil {
		l(kind, text)
	}
}

// PickLog is the first non-nil optional eventLog argument, so parsers called without one stay silent.
func PickLog(ls []StreamLog) StreamLog {
	for _, l := range ls {
		if l != nil {
			return l
		}
	}
	return nil
}

// logEvent writes `▸ kind: text` (one redacted, capped line) to ctx's log; a no-op without a sink.
func LogEvent(ctx context.Context, kind, text string) {
	if s := StderrLogFrom(ctx); s != nil {
		s.writeEvent(kind, text)
	}
}

func (s *LogSink) writeEvent(kind, text string) {
	// Redact before collapsing (multi-line secrets) and before capping (a cut secret would no longer match).
	text = strings.Join(strings.Fields(shapes.Redact(text)), " ")
	if text == "" {
		return
	}
	line := "▸ " + kind + ": " + textutil.Truncate(text, eventTextCap)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openLocked()
	s.writeLocked([]byte(line))
}

// Stream opens the log (first call) and returns a line-buffering writer for one child stream.
func (s *LogSink) Stream() io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openLocked()
	w := &logLineWriter{sink: s}
	s.streams = append(s.streams, w)
	return w
}

func (s *LogSink) openLocked() {
	if s.f == nil && !s.failed {
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			s.failed = true // fail-open: the log is for watching, never a gate on the run
		}
		s.f = f
	}
}

// Close flushes every stream's partial last line and closes the file.
func (s *LogSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.streams {
		s.writeLocked(w.buf)
		w.buf = nil
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	s.failed = true
}

func (s *LogSink) writeLocked(b []byte) {
	if len(b) == 0 || s.f == nil {
		return
	}
	line := shapes.Redact(string(b))
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, _ = s.f.WriteString(line)
}

// logLineWriter buffers to newlines so a secret split across Writes is redacted whole.
type logLineWriter struct {
	sink *LogSink
	buf  []byte
}

func (w *logLineWriter) Write(p []byte) (int, error) {
	w.sink.mu.Lock()
	defer w.sink.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.sink.writeLocked(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) >= logLineCap {
		w.sink.writeLocked(w.buf)
		w.buf = nil
	}
	return len(p), nil // never an error: a log failure must not abort the child's copy
}
