package ask

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
)

func TestSpecDryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	spec := `{"agent":"claude","fallback":"none","prompt":"say ok"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--spec", spec, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "agent:") || !strings.Contains(out, "argv:") || !strings.Contains(out, "say ok") {
		t.Fatalf("exit = %d, want 0 with the route block\nout=%s\nstderr=%s", code, out, stderr)
	}
	if n := len(agent.RecentRecords(2)); n != 0 {
		t.Fatalf("%d dispatch records saved, want none", n)
	}
}
