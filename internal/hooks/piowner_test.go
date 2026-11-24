package hooks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestPiSpecArgvAddsReportPath(t *testing.T) {
	argv, err := piSpecArgv(map[string]any{"agent": "claude"}, "Unit One")
	if err != nil {
		t.Fatal(err)
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
