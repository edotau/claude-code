package tree

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

func TestModelBudgetDropOrder(t *testing.T) {
	const model = "databricks-claude-sonnet-5-5"
	for _, width := range []int{0, 80, 120} {
		var l layout
		l.add(levelSubagent, "●", "opencode", model, "↑21.4k", "↓3.3k", "", "AGE", "IDLE", strings.Repeat("task ", 20))
		l.add(levelSubagent, "●", "opencode", "sonnet", "↑1k", "↓0", "Σ3k", "AGE", "IDLE", "parent task")
		lines := l.lines(width, "")
		if !strings.Contains(lines[0], model) || !strings.Contains(lines[1], "Σ3k") {
			t.Fatalf("width %d: model or parent total missing: %q", width, lines)
		}
		if width == 80 {
			if strings.Contains(lines[0], "AGE") || strings.Contains(lines[0], "IDLE") {
				t.Errorf("narrow frame kept durations: %q", lines[0])
			}
			if strings.Index(lines[0], "task") >= strings.Index(lines[1], "parent") {
				t.Errorf("leaf did not reclaim total padding: %q", lines)
			}
		} else if !strings.Contains(lines[0], "AGE") || !strings.Contains(lines[0], "IDLE") {
			t.Errorf("width %d dropped durations: %q", width, lines[0])
		}
	}
}

func TestModelBudgetClipsPathologicalIDLast(t *testing.T) {
	var l layout
	model := strings.Repeat("界", 30)
	l.add(levelSubagent, "●", "opencode", model, "↑21.4k", "↓3.3k", "Σ3k", "AGE", "IDLE", "task needs clipping here")
	line := l.lines(80, "")[0]
	if textutil.DisplayWidth(line) != 80 || strings.Contains(line, model) || !strings.HasSuffix(line, "task ne…") {
		t.Errorf("model did not yield eight task columns: %q", line)
	}
}
