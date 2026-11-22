package tree

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
)

// A carried pair is one row: the carry id and elapsed, the child's model, tokens and route; Pi shows as "codex→pi".
func TestFoldCarriesOneRowPerJob(t *testing.T) {
	now := time.Now()
	outer := Dispatch{ID: "1-codex", Agent: "codex", Model: "auto", Status: statusRunning, Duration: "2m43s", carry: true, launch: "dev"}
	child := Dispatch{ID: "2-codex", Agent: "codex", Model: "databricks-gpt-6-astra", Status: statusRunning, Parent: "1-codex",
		Usage: &wire.Usage{InputTokens: 1_200_000, OutputTokens: 9100}, LastSignal: now.Add(-7 * time.Second)}
	rows := jobRows([]Dispatch{outer, child})
	if len(rows) != 1 || rows[0].ID != "1-codex" {
		t.Fatalf("rows = %+v, want the one carry row", rows)
	}
	want := []string{"codex", "gpt-6-astra · dev", "↑1.2M", "↓9.1k", "7s", "2m43s"}
	if got := jobCells(rows[0].Dispatch, now, nil); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cells = %q, want %q", got, want)
	}
	pi := Dispatch{ID: "3-pi", Agent: "pi", Model: "databricks-claude-opus-5-5", Status: statusRunning, Parent: "1-codex"}
	if rows := jobRows([]Dispatch{outer, child, pi}); len(rows) != 1 || rows[0].Agent != "codex→pi" {
		t.Errorf("takeover rows = %+v, want one codex→pi row", rows)
	}
}

// The route shown is the launch's: record route, then request, then the carried line's --profile, before any pin.
func TestLaunchLabel(t *testing.T) {
	for _, c := range []struct {
		name string
		rec  agent.Record
		want string
	}{
		{"lead", agent.Record{Request: agent.RunOptions{Lead: "qa"}}, "qa"},
		{"carry", agent.Record{Request: agent.RunOptions{Carry: true, CarryArgv: []string{"ask", "--profile", "dev", "-"}}}, "dev"},
		{"overlay", agent.Record{Request: agent.RunOptions{CarryArgv: []string{"--overlay", `{"provider":"databricks","profile":"prod"}`}}}, "prod"},
		{"none", agent.Record{}, ""},
	} {
		if got := launchLabel(c.rec); got != c.want {
			t.Errorf("%s: launchLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBriefTaskSkipsHeader(t *testing.T) {
	if got := briefTask("Owned: [\"a\"]\nMode: x\n\nTask:\nfix it\nnow"); got != "fix it\nnow" {
		t.Errorf("briefTask = %q", got)
	}
	if got := briefTask("plain prompt"); got != "plain prompt" {
		t.Errorf("briefTask(plain) = %q", got)
	}
}

// Through the real renderer at 80 columns: rows never wrap, every column lines up, a queued row spans the tokens.
func TestJobRowsFitAndAlign(t *testing.T) {
	now := time.Now()
	use := &wire.Usage{InputTokens: 2_300_000, OutputTokens: 14_000}
	ds := []Dispatch{
		{ID: "a", Agent: "codex", Model: "databricks-gpt-6-astra", Status: statusRunning, Duration: "2m43s", launch: "dev",
			Usage: use, LastSignal: now.Add(-7 * time.Second)},
		{ID: "b", Agent: "opencode", Model: "databricks-claude-sonnet-5-5-a-very-long-model-id", Status: statusRunning,
			Duration: "1h2m", launch: "qa", Usage: use, LastSignal: now.Add(-time.Second)},
		{ID: "c", Agent: "gemini", Model: "gemini-4-argon", Status: statusRunning, Duration: "9s", launch: "prod",
			queued: "databricks:prod", queuedAt: now.Add(-70 * time.Second)},
	}
	var b strings.Builder
	renderTree(&b, &Tree{Dispatches: ds}, now, 80, "")
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")[1:]
	if len(lines) != 3 || !strings.HasPrefix(lines[2], "  ○  gemini") || !strings.Contains(lines[2], "queued 1m10s (pool databricks:prod)") {
		t.Fatalf("rows:\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		if n := textutil.DisplayWidth(line); n > 80 {
			t.Errorf("row is %d columns, want <= 80: %q", n, line)
		}
	}
	assertAligned(t, 80, "job", lines, []int{0, 1, 2}, nil)
	if !strings.Contains(lines[0], "gpt-6-astra · dev") || !strings.Contains(lines[0], "↑2.3M  ↓14k  7s  2m43s") {
		t.Errorf("row 0 = %q, want its whole model and route, then tokens side by side", lines[0])
	}
	ends := func(line string) int { return textutil.DisplayWidth(line) }
	if ends(lines[0]) != ends(lines[1]) || ends(lines[1]) != ends(lines[2]) {
		t.Errorf("elapsed columns do not end together:\n%s", strings.Join(lines, "\n"))
	}
}
