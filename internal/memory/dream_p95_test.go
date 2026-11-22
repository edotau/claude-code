package memory

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

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
