package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordTelemetryAppendsOneLinePerRow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	defer TelemetryDirForTest(dir)()
	RecordTelemetry([]TelemetryRow{{"Stop", "a", OutcomeOK, 1500 * time.Microsecond}, {"Stop", "b", OutcomeDeny, time.Millisecond}})
	RecordTelemetry([]TelemetryRow{{"Stop", "c", OutcomePanic, 0}})
	files, _ := filepath.Glob(filepath.Join(dir, "telemetry-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"module":"a"`) || !strings.Contains(lines[0], `"ms":1.5`) {
		t.Errorf("lines = %q", lines)
	}
	checkTelemetryStorageBounds(t, dir, files[0])
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 && filepath.Separator == '/' {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func checkTelemetryStorageBounds(t *testing.T, dir, file string) {
	t.Helper()
	old := filepath.Join(dir, "telemetry-"+time.Now().AddDate(0, 0, -15).Format("20060102")+".jsonl")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "telemetry-"+time.Now().Format("20060102")+".pruned")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	pruneTelemetry(dir, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired telemetry remains: %v", err)
	}
	if err := os.Truncate(file, telemetryDailyBytes); err != nil {
		t.Fatal(err)
	}
	RecordTelemetry([]TelemetryRow{{"Stop", "full", OutcomeOK, 0}})
	if st, err := os.Stat(file); err != nil || st.Size() != telemetryDailyBytes {
		t.Fatalf("daily ceiling: stat %v, err %v", st, err)
	}
}

func TestRecordTelemetryOffAndTestBinaryWriteNothing(t *testing.T) {
	RecordTelemetry([]TelemetryRow{{"Stop", "a", OutcomeOK, 0}}) // test binary: no directory, no write
	dir := t.TempDir()
	defer TelemetryDirForTest(dir)()
	t.Setenv(telemetryEnv, "off")
	RecordTelemetry([]TelemetryRow{{"Stop", "a", OutcomeOK, 0}})
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("off wrote %v", ents)
	}
}

func BenchmarkRecordTelemetry(b *testing.B) {
	defer TelemetryDirForTest(b.TempDir())()
	rows := make([]TelemetryRow, 8)
	for i := range rows {
		rows[i] = TelemetryRow{"UserPromptSubmit", "memory-recall", OutcomeOK, time.Millisecond}
	}
	for b.Loop() {
		RecordTelemetry(rows)
	}
}
