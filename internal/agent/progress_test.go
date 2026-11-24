package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
)

// ProgressOf and Line label a running dispatch from its own files in the five shapes, drop a missing file, and clip.
func TestProgressLine(t *testing.T) {
	setConfigDir(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, agent, model, log string
		elapsed, ago            time.Duration
		tools                   int // -1: no events file
		width                   int
		want                    string
	}{
		{"healthy", "codex", "gpt-6.1-sol", "▸ tool: /bin/zsh -lc 'go test ./...'\n", 252 * time.Second, 8 * time.Second, 23, 80,
			"codex · gpt-6.1-sol · 4m12s · last event 8s ago · 23 tools · go test ./..."},
		{"quiet", "opencode", "databricks-gpt-6-1-sol", "▸ tool: read\n", 220 * time.Second, 151 * time.Second, 17, 80,
			"opencode · databricks-gpt-6-1-sol · 3m40s · quiet 2m31s · 17 tools · read"},
		{"hung", "codex", "gpt-6.1-sol", "▸ tool: go test\n", 580 * time.Second, 362 * time.Second, 23, 80,
			"codex · gpt-6.1-sol · 9m40s · ⚠ no event 6m02s · 23 tools · last: go test"},
		{"cannot tell", "mystery", "gemini-4-argon", "", 550 * time.Second, 550 * time.Second, 0, 80,
			"mystery · gemini-4-argon · 9m10s · no live signal from mystery · 0 tools"},
		{"no signal past", "mystery", "gemini-4-argon", "", 662 * time.Second, 662 * time.Second, 0, 80,
			"mystery · gemini-4-argon · 11m02s · ⚠ no live signal past 10m15s"},
		{"no events file", "claude", "sonnet", "▸ tool: Read\n", 30 * time.Second, 5 * time.Second, -1, 80,
			"claude · sonnet · 30s · last event 5s ago · Read"},
		{"clip", "codex", "gpt-6.1-sol", "▸ tool: go test ./... -run TestSomethingVeryLong\n", 252 * time.Second, 8 * time.Second, 23, 80,
			"codex · gpt-6.1-sol · 4m12s · last event 8s ago · 23 tools · go test ./... -run…"},
		{"narrow", "codex", "gpt-6.1-sol", "▸ tool: go test\n", 252 * time.Second, 8 * time.Second, 23, 30,
			"codex · gpt-6.1-sol · 4m12s ·…"},
	}
	for i, c := range cases {
		r := NewRecord(c.agent, RunOptions{Model: c.model}, now.Add(-c.elapsed))
		r.ID = "prog" + string(rune('a'+i))
		r.Log = filepath.Join(t.TempDir(), "x.log")
		writeAged(t, r.Log, c.log, now.Add(-c.ago))
		if c.tools >= 0 {
			ev := `{"type":"start"}` + "\n" + strings.Repeat(`{"type":"tool_use"}`+"\n", c.tools)
			writeAged(t, r.EventsPath(), ev, now.Add(-c.ago))
		}
		got := ProgressOf(r, now).Line(c.width)
		if got != c.want || textutil.DisplayWidth(got) > c.width {
			t.Errorf("%s: line = %q, want %q", c.name, got, c.want)
		}
	}
}

// Live usage and the last tool's duration join the line; the duration yields to the task text, no usage adds nothing.
func TestProgressLineTokens(t *testing.T) {
	p := Progress{Agent: "pi", Elapsed: time.Minute, Tools: 21, Stream: true, quietAt: time.Hour, hungAt: 2 * time.Hour,
		Doing: "go test", In: 12400, Out: 350, ToolMs: 1500}
	want := "pi · 1m00s · last event 0s ago · 21 tools · ↑12.4k ↓350 · tool 2s · go test"
	if got := p.Line(80); got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	p.Doing = "a very long task text that needs room"
	if got := p.Line(80); strings.Contains(got, "tool 2s") || !strings.HasSuffix(got, "a very long task te…") {
		t.Errorf("long task line = %q", got)
	}
	p.In, p.Out, p.ToolMs = 0, 0, 0
	if got := p.Line(80); strings.Contains(got, "↑") {
		t.Errorf("no-usage line = %q", got)
	}
}

func writeAged(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestProgressCountsStreamPastChunkAndLongLines(t *testing.T) {
	old := progressChunk
	progressChunk = 16
	defer func() { progressChunk = old }()
	ev := strings.Repeat(`{"type":"tool_use"}`+"\n", 50) // marks straddle every 16-byte chunk
	if n, err := countToolEvents(strings.NewReader(ev)); err != nil || n != 50 {
		t.Fatalf("tool events = %d, %v; want 50", n, err)
	}
	log := "▸ step: a done\n▸ step: " + strings.Repeat("x", 100) + " done\n" + strings.Repeat("y", 100) + " done\n▸ step: b\n"
	if n, err := countDoneSteps(strings.NewReader(log)); err != nil || n != 2 {
		t.Fatalf("done steps = %d, %v; want 2", n, err)
	}
}

// Tokens are never clipped: a healthy "last event", then the tool count, yield first.
func TestProgressLineTokensKeptWhenWide(t *testing.T) {
	p := Progress{Agent: "pi", Model: "databricks-claude-sonnet-5-5", Elapsed: 14 * time.Second, Quiet: 13 * time.Second,
		Tools: 1, Stream: true, quietAt: time.Hour, hungAt: 2 * time.Hour, Doing: "bash", In: 4200, Out: 74}
	if got := p.Line(80); !strings.Contains(got, "↑4.2k ↓74") || strings.Contains(got, "last event") || textutil.DisplayWidth(got) > 80 {
		t.Errorf("line = %q", got)
	}
	p.Quiet, p.quietAt, p.Model, p.In, p.Out = 3*time.Hour, time.Minute, "databricks-claude-sonnet-5-5-very-long", 12_000_000, 0
	got := p.Line(80)
	if !strings.Contains(got, "↑12M ↓0") || strings.Contains(got, "1 tools") || textutil.DisplayWidth(got) > 80 {
		t.Errorf("large line = %q", got)
	}
	p.Out = 12345
	if got := p.Line(80); !strings.HasSuffix(got, "↑12M ↓12.3k") || textutil.DisplayWidth(got) > 80 {
		t.Errorf("long-model line = %q", got)
	}
}

// A tool duration prints whole or not at all.
func TestProgressLineToolDurWhole(t *testing.T) {
	p := Progress{Agent: "pi", Elapsed: time.Minute, Tools: 1, Stream: true, quietAt: time.Hour, hungAt: 2 * time.Hour, ToolMs: 850}
	if got := p.Fragment(30); strings.Contains(got, "tool 8") && !strings.Contains(got, "tool 850ms") {
		t.Errorf("partial duration = %q", got)
	}
}

// A no-stream agent with live usage is alive: its tokens show instead of "no live signal".
func TestProgressOfActivitySeen(t *testing.T) {
	r := Record{ID: "x", Agent: "copilot", Time: time.Now().Add(-time.Hour), Status: "running",
		Activity: &Activity{Usage: wire.Usage{InputTokens: 1500, OutputTokens: 20}}}
	if got := ProgressOf(r, time.Now()).Line(80); !strings.Contains(got, "↑1.5k ↓20") || strings.Contains(got, "no live") {
		t.Errorf("line = %q", got)
	}
}
