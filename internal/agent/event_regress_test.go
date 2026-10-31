package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readEvents decodes the event file at path, one Event per line.
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for ln := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var ev Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("bad event line %q: %v", ln, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// A secret split across two text deltas must land redacted in the event file, never raw.
func TestEventLogRedactsSecretSplitAcrossDeltas(t *testing.T) {
	tok := "ghp_" + strings.Repeat("a", 36)
	path := filepath.Join(t.TempDir(), "x.events.jsonl")
	l := NewEventLog(path, "id1")
	_ = l.Emit(Event{Type: EventText, Text: "token " + tok[:20]})
	_ = l.Emit(Event{Type: EventText, Text: tok[20:] + " end\n"})
	l.Close()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), tok[:20]) {
		t.Fatalf("event file leaks the split secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "end") {
		t.Fatalf("text lost:\n%s", raw)
	}
}

// Held text is flushed before the next non-text event, so the file reads text then tool_use.
func TestEventLogFlushesHeldTextBeforeToolUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.events.jsonl")
	l := NewEventLog(path, "id1")
	_ = l.Emit(Event{Type: EventStart})
	_ = l.Emit(Event{Type: EventText, Text: "no newline yet"})
	_ = l.Emit(toolEvent("Bash", "t1"))
	_ = l.Emit(Event{Type: EventResult})
	l.Close()
	var got []string
	for _, ev := range readEvents(t, path) {
		got = append(got, ev.Type)
	}
	if want := "start,text,tool_use,result"; strings.Join(got, ",") != want {
		t.Fatalf("event order = %v, want %s", got, want)
	}
}

// Each fallback hop emits exactly one retry event with the documented text.
func TestRunWithFallbackEmitsOneRetryPerHop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	first := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("boom")}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	var evs []Event
	sink := sinkFunc(func(ev Event) error { evs = append(evs, ev); return nil })
	c := Chain{Want: "codex", Dispatches: []Dispatch{{Agent: first}, {Agent: second}}}
	if _, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, sink, nil); err != nil {
		t.Fatal(err)
	}
	var retries []string
	for _, ev := range evs {
		if ev.Type == EventRetry {
			retries = append(retries, ev.Text)
		}
	}
	if len(retries) != 1 || retries[0] != "fallback codex → gemini: codex: failed: boom" {
		t.Fatalf("retry events = %q, want one %q", retries, "fallback codex → gemini: codex: failed: boom")
	}
}

type sinkFunc func(Event) error

func (f sinkFunc) Emit(ev Event) error { return f(ev) }

// Two assistant turns: the second text event carries Sep, and the plain text sink puts a newline between.
func TestClaudeStreamTurnSeparator(t *testing.T) {
	stream := `{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"a"}]}}` + "\n" +
		`{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"b"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"b"}` + "\n"
	var seps []bool
	var out strings.Builder
	text := NewTextSink(&out)
	sink := sinkFunc(func(ev Event) error {
		if ev.Type == EventText {
			seps = append(seps, ev.Sep)
		}
		return text.Emit(ev)
	})
	if _, err := parseClaudeStream(newBR(stream), sink); err != nil {
		t.Fatal(err)
	}
	if len(seps) != 2 || seps[0] || !seps[1] {
		t.Fatalf("Sep flags = %v, want [false true]", seps)
	}
	if out.String() != "a\nb" {
		t.Fatalf("text sink = %q, want %q", out.String(), "a\nb")
	}
}

func TestEventLogFlushesEachEventBeforeClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.events.jsonl")
	l := NewEventLog(path, "d1")
	defer l.Close()
	_ = l.Emit(Event{Type: EventStart, Agent: "codex", DispatchID: "d1", Status: "running"})
	_ = l.Emit(Event{Type: EventText, Text: "line one\n"})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"start"`) || !strings.Contains(string(b), "line one") {
		t.Fatalf("live reader sees %q before Close", b)
	}
}

// A secret inside the result envelope's structured value lands redacted in the event file, nested values too.
func TestEventLogRedactsStructured(t *testing.T) {
	tok := "ghp_" + strings.Repeat("b", 36)
	path := filepath.Join(t.TempDir(), "x.events.jsonl")
	l := NewEventLog(path, "id1")
	env := Envelope{Answer: "ok", Structured: json.RawMessage(`{"a":{"list":["` + tok + `",9007199254740993]}}`)}
	_ = l.Emit(ResultEvent(env))
	l.Close()
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) {
		t.Fatalf("event file leaks the structured secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("structured number altered:\n%s", raw)
	}
}

// resultProbe records the on-disk record status at the moment the result event is emitted.
type resultProbe struct {
	id     string
	status string
}

func (p *resultProbe) Emit(ev Event) error {
	if ev.Type == EventResult {
		got, _ := LoadRecord(p.id)
		p.status = got.Status
	}
	return nil
}

// A tail follower exits on a finished record, so the result event must be written before the record leaves running.
func TestDispatchWritesResultBeforeFinishedRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := ClaimRecord("codex", RunOptions{}, time.Now())
	probe := &resultProbe{id: rec.ID}
	_, err := RunRecorded(context.Background(), &rec, "codex", probe, RunRecordedOpts{},
		func(context.Context, Sink) (Result, error) {
			return Result{Agent: "codex", Status: StatusOK, Answer: "hi"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if probe.status != "running" {
		t.Fatalf("record status at result emit = %q, want running", probe.status)
	}
	if got, _ := LoadRecord(rec.ID); got.Status != StatusOK {
		t.Fatalf("final record status = %q, want %q", got.Status, StatusOK)
	}
}
