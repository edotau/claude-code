package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestHookTelemetryChecks(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(paths.StateDir(), "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var rows strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&rows, `{"module":"gate","outcome":"ok","ms":%d}`+"\n", i)
	}
	rows.WriteString(`{"module":"slow","outcome":"overrun","ms":200}` + "\n")
	rows.WriteString(`{"module":"slow","outcome":"deny","ms":10}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "telemetry-20261118.jsonl"), []byte(rows.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "telemetry-20261111.jsonl"), []byte(rows.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := hookTelemetryChecks(now)
	if len(got) != 3 || got[1].Name != "hook gate" || got[1].Detail != "20 | 0 | 0 | 19.00" ||
		got[2].Name != "hook slow" || got[2].Detail != "2 | 1 | 1 | 200.00" {
		t.Fatalf("unexpected telemetry rows: %+v", got)
	}
}
