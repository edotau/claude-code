package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	_ = l.Emit(ToolEvent("Bash", "t1"))
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

func TestEventLogRejectsTextPastCap(t *testing.T) {
	full := strings.Repeat("x", eventsTextCap)
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"oversized first chunk", []string{full + "x", "kept"}, "kept"},
		{"exact cap accepted", []string{full}, full},
		{"chunk after exact cap", []string{full, "overflow"}, full},
		{"crossing chunk dropped whole", []string{full[:eventsTextCap-1], "xy", "z"}, full[:eventsTextCap-1] + "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.events.jsonl")
			l := NewEventLog(path, "id1")
			t.Cleanup(l.Close)
			for _, chunk := range tc.chunks {
				if err := l.Emit(Event{Type: EventText, Text: chunk}); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.Emit(Event{Type: EventResult}); err != nil {
				t.Fatal(err)
			}
			l.Close()
			events := readEvents(t, path)
			var text strings.Builder
			for _, ev := range events {
				if ev.Type == EventText {
					text.WriteString(ev.Text)
				}
			}
			if text.String() != tc.want {
				t.Errorf("stored text = %d bytes, want %d bytes with only accepted chunks", text.Len(), len(tc.want))
			}
			if len(events) == 0 || events[len(events)-1].Type != EventResult {
				t.Error("text cap dropped the non-text result event")
			}
		})
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

// A progress mark flips runSink's flag with no text or tool call; an empty text event does not.
func TestRunSinkCountsProgressMark(t *testing.T) {
	rs := &RunSink{Sink: Discard, Agent: "x"}
	_ = rs.Emit(Event{Type: EventText})
	if rs.Progressed {
		t.Fatal("empty text counted as progress")
	}
	_ = rs.Emit(Event{Type: EventProgress})
	if !rs.Progressed {
		t.Fatal("progress mark not counted")
	}
}

// The mark reaches neither the text sink nor the event file.
func TestProgressMarkIsInvisible(t *testing.T) {
	var sb strings.Builder
	_ = NewTextSink(&sb).Emit(Event{Type: EventProgress})
	path := filepath.Join(t.TempDir(), "x.events.jsonl")
	l := NewEventLog(path, "id1")
	_ = l.Emit(Event{Type: EventStart})
	_ = l.Emit(Event{Type: EventProgress})
	l.Close()
	if sb.Len() != 0 {
		t.Errorf("text sink wrote %q; want nothing", sb.String())
	}
	for _, ev := range readEvents(t, path) {
		if ev.Type == EventProgress {
			t.Errorf("event file kept the mark: %+v", ev)
		}
	}
}
