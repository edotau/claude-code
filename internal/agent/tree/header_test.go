package tree

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/dev/review/bench"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

func TestHeaderLines(t *testing.T) {
	h := headerStatus{
		bench: bench.Status{LaneCap: 3, Lanes: []bench.StatusLane{{Name: "bench-a"}, {Name: "autofix-b"}},
			Queue:  &bench.StatusQueue{Total: 5, Counts: map[string]int{"fix": 3}},
			Budget: &bench.StatusBudget{Tokens: 30, TokenLimit: 100}},
		serve: serve.Status{Pools: []serve.PoolRow{{Pool: "codex", Running: 1, Cap: 2,
			Waiting: []serve.Waiter{{ID: "one"}}}}}, served: true,
	}
	for _, width := range []int{80, 40} {
		lines := headerLines(h, width)
		if len(lines) != 2 || !strings.Contains(lines[0], "codex 1/2 wait 1") ||
			!strings.Contains(lines[1], "30/100") || (width == 40 && !strings.Contains(lines[1], "q5")) {
			t.Fatalf("width %d: %q", width, lines)
		}
		for _, line := range lines {
			if n := textutil.DisplayWidth(line); n > width {
				t.Fatalf("width %d: line uses %d cells: %q", width, n, line)
			}
		}
	}
}

func TestHeaderErrors(t *testing.T) {
	h := headerStatus{serveError: true, bench: bench.Status{
		BudgetError: "private path", LanesError: "private path", QueueError: "private path"}}
	lines := headerLines(h, 80)
	if lines[0] != "serve: unreadable" || !strings.Contains(lines[1], "budget: unreadable") ||
		strings.Contains(strings.Join(lines, " "), "private path") {
		t.Fatalf("header errors: %q", lines)
	}
	h.serveError = false
	if got := headerLines(h, 80)[0]; got != "serve: off" {
		t.Fatalf("no daemon: %q", got)
	}
}
