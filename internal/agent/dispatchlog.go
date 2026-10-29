package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/textutil"
)

// Record is one `ask` dispatch: written as "running" before the run, so a killed run stays visible.
type Record struct {
	Schema         int       `json:"schema"`
	ID             string    `json:"id"`
	Time           time.Time `json:"time"`
	Agent          string    `json:"agent"`
	Request        Request   `json:"request"`
	Status         string    `json:"status"` // running | ok | error
	Error          string    `json:"error,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`         // as the runner reported it; may be empty
	Invoked        string    `json:"invoked_model,omitempty"` // the model the last hop was asked to run; Request keeps the original
	SessionID      string    `json:"session_id,omitempty"`
	DurationMs     int64     `json:"duration_ms,omitempty"`
	Fallback       string    `json:"fallback,omitempty"` // the original runner's name, when it ran instead
	FallbackReason string    `json:"fallback_reason,omitempty"`
	RanAs          string    `json:"ran_as,omitempty"`   // the runner that answered, when a fallback leg did
	Repo           string    `json:"repo,omitempty"`     // main checkout root the job belongs to
	PID            int       `json:"pid,omitempty"`      // pid of the detached `claude-code ask --job` child
	Log            string    `json:"log,omitempty"`      // absolute path of the job's combined stdout+stderr log
	Progress       string    `json:"progress,omitempty"` // multi-part review job: "part 2/3"
	Owner          int       `json:"owner,omitempty"`    // launching Claude Code session pid (inflight.SessionOwner)
}

// Alive reports whether this dispatch is still running: pid alive, or not yet recorded within a minute.
func (r Record) Alive() bool {
	if r.Status != "running" {
		return false
	}
	if r.PID > 0 {
		return proc.Alive(r.PID)
	}
	return time.Since(r.Time) < time.Minute
}

// LogPath is the dispatch's log file: Log when set, else <id>.log beside the record.
func (r Record) LogPath() string { return cmp.Or(r.Log, filepath.Join(DispatchDir(), r.ID+".log")) }

// dispatchKeep bounds the log; the oldest records beyond it are pruned on every write.
var dispatchKeep = 200

// DispatchDir is the directory holding dispatch records and job logs.
func DispatchDir() string { return filepath.Join(paths.StateDir(), "dispatch") }

// NewRecord stamps an id that sorts by time and names the agent: 20261016T210512.123-codex.
func NewRecord(agent string, req Request, now time.Time) Record {
	return Record{Schema: DispatchSchema, ID: recordID(now, agent), Time: now.UTC(), Agent: agent, Request: req, Status: "running"}
}

// recordID is the id NewRecord stamps for agent at now.
func recordID(now time.Time, agent string) string {
	return now.UTC().Format("20060102T150405.000") + "-" + agent
}

// Save writes the record (0600) and prunes past dispatchKeep. The prompt is kept verbatim so a retry is exact.
func (r Record) Save() error {
	dir := DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r.Schema = DispatchSchema
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := paths.AtomicWrite(filepath.Join(dir, r.ID+".json"), b, 0o600); err != nil {
		return err
	}
	pruneDispatches(dir)
	return nil
}

// Elapsed is the run time as text; "" for a record with no duration (running, or written before duration_ms).
func (r Record) Elapsed() string {
	if r.DurationMs <= 0 {
		return ""
	}
	return (time.Duration(r.DurationMs) * time.Millisecond).String()
}

// Finish records the outcome of the run.
func (r *Record) Finish(res Result, err error, took time.Duration) {
	r.Status, r.Provider, r.Model, r.SessionID = "ok", res.Provider, res.Model, res.SessionID
	r.Invoked = res.Invoked
	r.Fallback, r.FallbackReason = res.Fallback, res.FallbackReason
	if res.Fallback != "" {
		r.RanAs = res.Agent
	}
	r.DurationMs = took.Milliseconds()
	if err != nil {
		r.Status, r.Error = "error", err.Error()
	}
}

func pruneDispatches(dir string) {
	names := recordNames(dir)
	excess := len(names) - dispatchKeep
	for _, name := range names {
		if excess <= 0 {
			return
		}
		if activeRecord(filepath.Join(dir, name)) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".json")+".log")) // ignore not-exist
		excess--
	}
}

// activeRecord keeps a live dispatch (see Record.Alive) out of the prune.
func activeRecord(path string) bool {
	r, err := readRecord(path)
	if err != nil {
		return errors.Is(err, schemas.ErrNewer) // a newer build's record is never pruned blind
	}
	return r.Alive()
}

// readRecord reads and unmarshals one record file by its full path.
func readRecord(path string) (Record, error) {
	var r Record
	b, err := os.ReadFile(path)
	if err == nil {
		_, err = schemas.Decode(dispatchFormat, b, &r)
	}
	return r, err
}

// recordNames lists record files oldest first (ids sort by time).
func recordNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// LoadRecord reads one record by id; a unique id prefix is accepted.
func LoadRecord(id string) (Record, error) {
	var match []string
	for _, n := range recordNames(DispatchDir()) {
		if strings.HasPrefix(n, id) {
			match = append(match, n)
		}
	}
	if len(match) != 1 {
		return Record{}, fmt.Errorf("dispatch %q: %d matches (see claude-code ask --log)", id, len(match))
	}
	return readRecord(filepath.Join(DispatchDir(), match[0]))
}

// RecentRecords returns up to n records, newest first.
func RecentRecords(n int) []Record {
	dir := DispatchDir()
	names := recordNames(dir)
	var out []Record
	for i := len(names) - 1; i >= 0 && len(out) < n; i-- {
		if r, err := readRecord(filepath.Join(dir, names[i])); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// logLineCap bounds one buffered log line; a longer run is redacted and written as its own chunk.
const logLineCap = 256 << 10

// LogSink tees a dispatch's child output, redacted line by line, into its log file (0600, append).
// Safe for the stdout and stderr streams of every fallback leg to share; a write failure never reaches the run.
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
func stderrLogFrom(ctx context.Context) *LogSink {
	c, _ := ctx.Value(logSinkKey{}).(logCarrier)
	return c.sink
}

// teeWriter returns w that also feeds ctx's log, or w itself when the dispatch has none.
func teeWriter(ctx context.Context, w io.Writer) io.Writer {
	if s := logFrom(ctx); s != nil {
		return io.MultiWriter(w, s.Stream())
	}
	return w
}

// eventLog writes one formatted event line to the dispatch log; a nil eventLog is a no-op.
type eventLog func(kind, text string)

// eventTextCap bounds the text of one `▸ kind: text` log line, in runes.
const eventTextCap = 300

// eventLogFor binds ctx's sink; nil when the dispatch has none.
func eventLogFor(ctx context.Context) eventLog {
	if stderrLogFrom(ctx) == nil {
		return nil
	}
	return func(kind, text string) { logEvent(ctx, kind, text) }
}

func (l eventLog) emit(kind, text string) {
	if l != nil {
		l(kind, text)
	}
}

// pickLog is the first non-nil optional eventLog argument, so parsers called without one stay silent.
func pickLog(ls []eventLog) eventLog {
	for _, l := range ls {
		if l != nil {
			return l
		}
	}
	return nil
}

// logEvent writes `▸ kind: text` (one redacted, capped line) to ctx's log; a no-op without a sink.
func logEvent(ctx context.Context, kind, text string) {
	if s := stderrLogFrom(ctx); s != nil {
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
