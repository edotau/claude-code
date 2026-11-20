package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{"cannot tell", "copilot", "gemini-4-argon", "", 550 * time.Second, 550 * time.Second, 0, 80,
			"copilot · gemini-4-argon · 9m10s · no live signal from copilot · 0 tools"},
		{"no signal past", "copilot", "gemini-4-argon", "", 662 * time.Second, 662 * time.Second, 0, 80,
			"copilot · gemini-4-argon · 11m02s · ⚠ no live signal past 10m15s"},
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
