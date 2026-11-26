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

func TestPiBriefArgvAddsReportPath(t *testing.T) {
	t.Setenv(route.OverlayVar, "")
	argv, err := piBriefArgv("fast", map[string]any{"prompt": "do x"}, "Unit One")
	if err != nil {
		t.Fatal(err)
	}
	var ov agentconfig.Overlay
	if i := slices.Index(argv, "--overlay"); i < 0 || json.Unmarshal([]byte(argv[i+1]), &ov) != nil ||
		ov.Agent != "claude" || ov.Tier != "fast" {
		t.Fatalf("argv = %q", argv)
	}
	if i := slices.Index(argv, "--brief"); i < 0 || i != len(argv)-2 {
		t.Fatalf("argv = %q, want --brief last", argv)
	}
	t.Setenv(route.OverlayVar, `{"runner":"own"}`)
	if inh, err := piBriefArgv("fast", map[string]any{}, "u"); err != nil || slices.Contains(inh, "--overlay") ||
		!slices.Contains(inh, "--agent") || !slices.Contains(inh, "--tier") {
		t.Fatalf("inherited overlay argv = %q, err = %v", inh, err)
	}
	var brief map[string]any
	if err := json.Unmarshal([]byte(argv[len(argv)-1]), &brief); err != nil {
		t.Fatal(err)
	}
	if _, hasAgent := brief["agent"]; hasAgent {
		t.Fatalf("brief must not carry config keys: %v", brief)
	}
	report, _ := brief["report"].(string)
	if filepath.Dir(report) != paths.MemorySubDir("reports") || !strings.Contains(report, "-unit-one-") {
		t.Fatalf("report = %q", report)
	}
}
