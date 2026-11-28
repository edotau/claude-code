package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/hooktelemetry"
	"github.com/edotau/claude-code/internal/platform/paths"
)

func TestHookTelemetryChecksSkipsProbeRows(t *testing.T) {
	const dayFile = "telemetry-20261118.jsonl"
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(paths.StateDir(), "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := `{"module":"guard","outcome":"deny","ms":1,"sid":"s1","verdict":"block"}` + "\n" +
		`{"module":"guard","outcome":"ok","ms":1,"sid":"probe-le"}` + "\n" +
		`{"module":"guard","outcome":"ok","ms":2,"sid":"a1b2c3d4"}` + "\n" +
		`{"module":"guard","outcome":"ok","ms":3,"sid":"ses_1a2b"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, dayFile), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	got := hookTelemetryChecks(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC))
	if len(got) != 2 || got[1].Detail != "2 | 0 | 0 | 3.00 | 3.00" || !strings.HasSuffix(got[0].Detail, "(+2 probe rows skipped)") {
		t.Fatalf("probe rows not skipped: %+v", got)
	}
}

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
	if len(got) != 3 || got[1].Name != "hook gate" || got[1].Detail != "20 | 0 | 0 | 11.00 | 19.00" ||
		got[2].Name != "hook slow" || got[2].Detail != "2 | 1 | 1 | 200.00 | 200.00" {
		t.Fatalf("unexpected telemetry rows: %+v", got)
	}
}

func TestDoctorHooksFlag(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	hookDir := filepath.Join(paths.StateDir(), "hooks")
	projDir := filepath.Join(cfg, "projects", "p1")
	for _, d := range []string{hookDir, projDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dayFile := "telemetry-" + time.Now().UTC().Format("20060102") + ".jsonl"
	var tel strings.Builder
	for range 120 {
		tel.WriteString(`{"module":"worktree-claim-check","outcome":"ok","ms":2,"sid":"a1b2c3d4"}` + "\n")
	}
	tel.WriteString(`{"module":"worktree-clean-end","outcome":"overrun","ms":1500,"sid":"a1b2c3d4"}` + "\n")
	if err := os.WriteFile(filepath.Join(hookDir, dayFile), []byte(tel.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctxLine := `{"attachment":{"type":"hook_additional_context","content":"<memory-recall>\n- [mem-1] rule one\n- [mem-1] rule one\n</memory-recall>"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projDir, "s1.jsonl"), []byte(ctxLine+ctxLine), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := scanHookContext(filepath.Join(cfg, "projects"), time.Now(), 7*24*time.Hour, time.Second)
	rep := hooktelemetry.Read(time.Now(), hooktelemetry.Activity)
	checks := buildHookAuditChecks(7, rep, ca)
	joined := fmt.Sprintf("%+v", checks)
	for _, want := range []string{"hook effectiveness", "hook context tokens", "optimize memory-recall", "optimize worktree-claim-check", "optimize worktree-clean-end latency"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
}
