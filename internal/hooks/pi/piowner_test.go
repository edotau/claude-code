package pi

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/route"
)

func TestPiSpecArgvAddsReportPath(t *testing.T) {
	t.Setenv(route.OverlayVar, "")
	argv, err := piSpecArgv(piRunOverlay("fast"), map[string]any{"agent": "claude"}, "Unit One")
	if err != nil {
		t.Fatal(err)
	}
	var ov agentconfig.Overlay
	if i := slices.Index(argv, "--overlay"); i < 0 || json.Unmarshal([]byte(argv[i+1]), &ov) != nil ||
		ov.Agent != "claude" || ov.Tier != "fast" {
		t.Fatalf("argv = %q", argv)
	}
	t.Setenv(route.OverlayVar, `{"runner":"own"}`)
	if inh, _ := piSpecArgv(piRunOverlay("fast"), map[string]any{}, "u"); slices.Contains(inh, "--overlay") {
		t.Fatalf("an inherited overlay must not be replaced: %q", inh)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(argv[len(argv)-1]), &spec); err != nil {
		t.Fatal(err)
	}
	report, _ := spec["report"].(string)
	if filepath.Dir(report) != paths.MemorySubDir("reports") || !strings.Contains(report, "-unit-one-") {
		t.Fatalf("report = %q", report)
	}
}
