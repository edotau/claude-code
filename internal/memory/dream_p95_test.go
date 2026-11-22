package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestQueueDreamBenchBoundsAndDedupes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	file := filepath.Join(paths.StateDir(), "bench", "queue.jsonl")
	seedDreamBenchQueue(t, file, now)
	if err := queueDreamBench(dreamBenchEntry{At: now, Source: "new", Module: "incoming"}); err != nil {
		t.Fatal(err)
	}
	if err := queueDreamBench(dreamBenchEntry{At: now.Add(time.Second), Source: "new", Module: "incoming", BeforeMS: 3}); err != nil {
		t.Fatal(err)
	}
	checkDreamBenchQueue(t, file)
}

func seedDreamBenchQueue(t *testing.T, file string, now time.Time) {
	t.Helper()
	var seed strings.Builder
	add := func(entry dreamBenchEntry) {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(&seed, string(data))
	}
	add(dreamBenchEntry{At: now.Add(-15 * 24 * time.Hour), Source: "old", Module: "expired"})
	add(dreamBenchEntry{At: now.Add(-time.Hour), Source: "rotation", Module: "duplicate", BeforeMS: 1})
	add(dreamBenchEntry{At: now.Add(-time.Minute), Source: "rotation", Module: "duplicate", BeforeMS: 2})
	for i := range 205 {
		add(dreamBenchEntry{At: now.Add(time.Duration(i-206) * time.Minute), Source: "rotation", Pkg: fmt.Sprintf("pkg-%03d", i)})
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(seed.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkDreamBenchQueue(t *testing.T, file string) {
	t.Helper()
	counts, lines := dreamBenchQueueCounts(t, file)
	if lines != dreamBenchQueueLimit {
		t.Fatalf("queue has %d lines, want %d", lines, dreamBenchQueueLimit)
	}
	if counts["old\x00expired"] != 0 || counts["rotation\x00duplicate"] != 1 || counts["new\x00incoming"] != 1 {
		t.Errorf("queue keys = %v", counts)
	}
	if counts["rotation\x00pkg-000"] != 0 || counts["rotation\x00pkg-204"] != 1 {
		t.Errorf("cap kept wrong rows: oldest=%d newest=%d", counts["rotation\x00pkg-000"], counts["rotation\x00pkg-204"])
	}
}

func dreamBenchQueueCounts(t *testing.T, file string) (map[string]int, int) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	counts := map[string]int{}
	for _, line := range lines {
		var entry dreamBenchEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		counts[dreamBenchQueueKey(entry)]++
		if entry.Source == "rotation" && entry.Module == "duplicate" && entry.BeforeMS != 2 {
			t.Errorf("kept older duplicate: %+v", entry)
		}
		if entry.Source == "new" && entry.BeforeMS != 3 {
			t.Errorf("kept older incoming entry: %+v", entry)
		}
	}
	return counts, len(lines)
}

func TestDreamP95Windows(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                  string
		after                 float64
		beforeRuns, afterRuns int
		flag                  bool
	}{
		{"regression", 12.01, 200, 200, true},
		{"exactly twenty percent", 12, 200, 200, false},
		{"too few before", 50, 199, 200, false},
		{"too few after", 50, 200, 199, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeDreamP95Rows(t, dir, now, tc.after, tc.beforeRuns, tc.afterRuns)
			flags, err := readDreamP95(dir, now)
			if err != nil {
				t.Fatal(err)
			}
			var want []dreamBenchEntry
			if tc.flag {
				want = []dreamBenchEntry{{At: now, Source: "dream-p95", Module: "memory-start", Pkg: "internal/hooks/memhook", BeforeMS: 10, AfterMS: tc.after}}
			}
			if !reflect.DeepEqual(flags, want) {
				t.Fatalf("flags = %+v, want %+v", flags, want)
			}
		})
	}
}

func writeDreamP95Rows(t *testing.T, dir string, now time.Time, after float64, beforeRuns, afterRuns int) {
	t.Helper()
	for _, w := range []struct {
		at   time.Time
		ms   float64
		runs int
	}{
		{now.Add(-2 * dreamP95Window), 10, beforeRuns}, {now.Add(-dreamP95Window), after, afterRuns},
	} {
		var rows strings.Builder
		for i := range w.runs {
			ms := w.ms
			if i >= w.runs-w.runs/20 {
				ms = 10000
			}
			fmt.Fprintf(&rows, "{\"t\":%q,\"module\":\"memory-start\",\"ms\":%g}\n", w.at.Format(time.RFC3339), ms)
		}
		fmt.Fprintf(&rows, "{\"t\":%q,\"module\":\"memory-start\",\"ms\":99999,\"sid\":\"livechec\"}\n", w.at.Format(time.RFC3339))
		write(t, filepath.Join(dir, "telemetry-"+w.at.Format("20060102")+".jsonl"), rows.String())
	}
	var outside strings.Builder
	for range 200 {
		for _, at := range []time.Time{now.Add(-2*dreamP95Window - time.Nanosecond), now, now.Add(time.Hour)} {
			row := dreamHookRow{T: at, Module: "memory-start", MS: &after}
			data, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&outside, string(data))
		}
	}
	write(t, filepath.Join(dir, "telemetry-"+now.Format("20060102")+".jsonl"), outside.String())
}
