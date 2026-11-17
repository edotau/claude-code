package ask

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

func TestResultCarriesObservedFactsOnlyWhenRecorded(t *testing.T) {
	t.Setenv("HARNESS_STATE_DIR", t.TempDir())
	wrote := []string{"/r/a.go", "/r/b.go", "/r/c.go"}
	rec := agent.Record{ID: "x-codex", Agent: "codex", Status: "ok", Parent: "p-1", Depth: 1, DurationMs: 130000,
		Model: "gpt", Route: "dbx:qa", Wrote: &wrote, Usage: &wire.Usage{InputTokens: 40000, OutputTokens: 1000}}
	text := resultText(rec, "answer\n")
	line := ""
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(l, "observed: ") {
			line = l
		}
	}
	if line != "observed: 3 files · 41k tok · depth 1 · 2m10s" || len(line) > 80 {
		t.Errorf("observed line = %q", line)
	}
	raw, _ := json.Marshal(agent.ObservedOf(rec))
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["parent"] != "p-1" || got["depth"] != 1.0 || got["files"] != 3.0 || got["route"] != "dbx:qa" || got["usage"] == nil {
		t.Errorf("observed json = %s", raw)
	}
	legacy := agent.Record{ID: "y-codex", Agent: "codex", Status: "ok"}
	if strings.Contains(resultText(legacy, "answer\n"), "observed:") {
		t.Error("legacy record printed an observed line")
	}
	if raw, _ := json.Marshal(agent.ObservedOf(legacy)); strings.Contains(string(raw), "files") || strings.Contains(string(raw), "parent") {
		t.Errorf("legacy observed json = %s", raw)
	}
}

func TestLastLinesDropsTrailingFooters(t *testing.T) {
	text := "one\ntwo\nthree\n\nask: ran on x · y\nask: dispatch 1-x\nsession: abc\nresume: x --resume abc\n"
	if got, want := lastLines(text, 2), []string{"two", "three"}; !slices.Equal(got, want) {
		t.Errorf("lastLines = %q, want %q", got, want)
	}
	if got := lastLines("ask: dispatch 1\n", 3); got == nil || len(got) != 0 {
		t.Errorf("footer-only text = %#v, want empty non-nil", got)
	}
	if got, want := lastLines("ask: mid\nlast\n", 5), []string{"ask: mid", "last"}; !slices.Equal(got, want) {
		t.Errorf("non-trailing footer-like line = %q, want %q", got, want)
	}
}

func TestLastLinesDropsTraceLinesWhenAnswerExists(t *testing.T) {
	text := "▸ assistant: draft\n▸ result: step finished\nalpha\nbeta\n▸ result: tail\nsession: s\n"
	if got, want := lastLines(text, 10), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Errorf("lastLines = %q, want only answer lines %q", got, want)
	}
	only := "▸ result: step\n▸ assistant: one\n▸ result: step\n▸ assistant: two\n"
	if got, want := lastLines(only, 10), []string{"▸ assistant: one", "▸ assistant: two"}; !slices.Equal(got, want) {
		t.Errorf("trace-only log = %q, want only its assistant lines %q", got, want)
	}
	bare := []string{"▸ result: a", "▸ result: b"}
	if got := lastLines("▸ result: a\n▸ result: b\n", 10); !slices.Equal(got, bare) {
		t.Errorf("log with no answer = %q, want %q", got, bare)
	}
}
