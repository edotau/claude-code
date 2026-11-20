package hooktelemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordAppendsOneLinePerRow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks")
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	deadline := time.Now().Add(time.Hour)
	recordTelemetry(dir, []Row{{"Stop", "a", OutcomeOK, 1500 * time.Microsecond, "", "", "", ""}, {"Stop", "b", OutcomeDeny, time.Millisecond, "", "", "", ""}}, now, deadline)
	recordTelemetry(dir, []Row{{"Stop", "c", OutcomePanic, 0, "", "", "", ""}}, now, deadline)
	files, _ := filepath.Glob(filepath.Join(dir, "telemetry-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"module":"a"`) || !strings.Contains(lines[0], `"ms":1.5`) {
		t.Errorf("lines = %q", lines)
	}
	checkTelemetryStorageBounds(t, dir, files[0], now)
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 && filepath.Separator == '/' {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

func checkTelemetryStorageBounds(t *testing.T, dir, file string, now time.Time) {
	t.Helper()
	old := filepath.Join(dir, "telemetry-"+now.AddDate(0, 0, -15).Format("20060102")+".jsonl")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "telemetry-"+now.Format("20060102")+".pruned")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	pruneTelemetry(dir, now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired telemetry remains: %v", err)
	}
	if err := os.Truncate(file, telemetryDailyBytes); err != nil {
		t.Fatal(err)
	}
	recordTelemetry(dir, []Row{{"Stop", "full", OutcomeOK, 0, "", "", "", ""}}, now, time.Now().Add(time.Hour))
	if st, err := os.Stat(file); err != nil || st.Size() != telemetryDailyBytes {
		t.Fatalf("daily ceiling: stat %v, err %v", st, err)
	}
}

func TestRecordKeepsRecordingPastTheOldCapAndMarksTheDrop(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	file := filepath.Join(dir, "telemetry-20261118.jsonl")
	if err := os.WriteFile(file, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	record := func(m string) {
		recordTelemetry(dir, []Row{{"Stop", m, OutcomeOK, 0, "", "", "", ""}}, now, time.Now().Add(time.Hour))
	}
	record("after-old-cap")
	if raw, _ := os.ReadFile(file); !strings.Contains(string(raw), "after-old-cap") {
		t.Fatal("a row past the old 4 MiB cap was dropped")
	}
	if err := os.Truncate(file, telemetryDailyBytes-telemetryMarkerReserve); err != nil {
		t.Fatal(err)
	}
	record("lost1")
	record("lost2")
	raw, _ := os.ReadFile(file)
	if n := strings.Count(string(raw), `"outcome":"`+OutcomeDropped+`"`); n != 1 || strings.Contains(string(raw), "lost") {
		t.Errorf("dropped markers = %d, lost rows present = %v", n, strings.Contains(string(raw), "lost"))
	}
	if st, _ := os.Stat(file); st.Size() > telemetryDailyBytes {
		t.Errorf("size %d exceeds the daily cap", st.Size())
	}
}

func TestPruneTelemetryBoundsTotalBytes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	for _, d := range []string{"20261110", "20261111", "20261118"} {
		f := filepath.Join(dir, "telemetry-"+d+".jsonl")
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(f, telemetryTotalBytes*2/5); err != nil {
			t.Fatal(err)
		}
	}
	pruneTelemetry(dir, now)
	left, _ := filepath.Glob(filepath.Join(dir, "telemetry-*.jsonl"))
	if len(left) != 2 || !strings.HasSuffix(left[0], "20261111.jsonl") {
		t.Errorf("left = %v, want the two newest", left)
	}
}

func TestRecordOffAndTestBinaryWriteNothing(t *testing.T) {
	Record([]Row{{"Stop", "a", OutcomeOK, 0, "", "", "", ""}}) // test binary: no directory, no write
	dir := t.TempDir()
	defer DirForTest(dir)()
	t.Setenv(telemetryEnv, "off")
	Record([]Row{{"Stop", "a", OutcomeOK, 0, "", "", "", ""}})
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("off wrote %v", ents)
	}
}

func BenchmarkRecord(b *testing.B) {
	defer DirForTest(b.TempDir())()
	rows := make([]Row, 8)
	for i := range rows {
		rows[i] = Row{"UserPromptSubmit", "memory-recall", OutcomeOK, time.Millisecond, "", "", "", ""}
	}
	for b.Loop() {
		Record(rows)
	}
}
