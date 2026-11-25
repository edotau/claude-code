package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestShadowPlanLogsOnlyDivergence(t *testing.T) {
	setConfigDir(t)
	log := filepath.Join(paths.StateDir(), "agent", "routeplan-shadow.jsonl")
	logDivergence(context.Background(), "codex", nil)
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a matching plan wrote a log line")
	}
	logDivergence(PeekContext(context.Background()), "codex", []string{"hop 1: plan pi@dev, chain codex@dev"})
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a peek wrote a log line")
	}
	logDivergence(context.Background(), "codex", []string{"hop 1: plan pi@dev, chain codex@dev"})
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 1 || !strings.Contains(string(b), "plan pi@dev") {
		t.Errorf("log = %q, want one divergence line", b)
	}
}
