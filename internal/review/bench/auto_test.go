package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func autoRow(at time.Time, mod func(*Score)) observation {
	s := Score{Samples: 5, CostSec: 1, Metrics: map[string]Spread{"ns/op": {Median: 1e6, CV: 1}}}
	if mod != nil {
		mod(&s)
	}
	return observation{At: at, Pkg: "internal/p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64), Score: &s}
}

func regressed(s *Score) {
	s.Deltas = map[string]Delta{"sec/op": {Old: "100", New: "110", Change: "+10.00%", Significant: true}}
}

func noisyRun(s *Score) { s.Metrics = map[string]Spread{"ns/op": {Median: 1e6, CV: 9}} }

func TestHistoryCandidates(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, kind, what string
		rows             []observation
	}{
		{"clean regression", "fix", "BenchmarkA regressed +10.00%", []observation{autoRow(now, regressed)}},
		{"minor regression", "", "", []observation{autoRow(now, func(s *Score) {
			s.Deltas = map[string]Delta{"sec/op": {Old: "100", New: "101", Change: "+1.00%", Significant: true}}
		})}},
		{"costly", "fix", "BenchmarkA is costly: 3.00 s per sample", []observation{autoRow(now, func(s *Score) { s.CostSec = 15 })}},
		{"noisy twice", "benchmark", "BenchmarkA is noisy twice in a row: rewrite the benchmark",
			[]observation{autoRow(now.Add(-time.Hour), noisyRun), autoRow(now, noisyRun)}},
		{"noisy once", "", "", []observation{autoRow(now.Add(-time.Hour), nil), autoRow(now, noisyRun)}},
		{"load held", "", "", []observation{autoRow(now, func(s *Score) { regressed(s); s.Flags = []string{"load"} })}},
		{"noisy after load", "", "", []observation{
			autoRow(now.Add(-time.Hour), func(s *Score) { s.Flags = []string{"load"} }), autoRow(now, noisyRun)}},
		{"empty", "", "", []observation{autoRow(now, func(s *Score) { regressed(s); s.Samples = 1 })}},
		{"no score", "", "", []observation{{At: now, Pkg: "internal/p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := historyCandidates(tc.rows)
			if tc.kind == "" {
				if len(got) != 0 {
					t.Fatalf("queued %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].What != tc.what || got[0].Source != "history" {
				t.Fatalf("got %+v, want %s %q", got, tc.kind, tc.what)
			}
		})
	}
}

func TestNextCandidate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	if err := appendHistory(filepath.Join(benchRoot(), "p"), []observation{autoRow(now, regressed)}); err != nil {
		t.Fatal(err)
	}
	writeBenchFile(t, "candidates.jsonl", `{"pkg":"internal/p","what":"x","calls_per_day":100}`)
	it, ok, err := NextCandidate(now)
	if err != nil || !ok || it.What != "BenchmarkA regressed +10.00%" || it.CallsPerDay != 100 || it.EstSavingMS != 0.1 {
		t.Fatalf("next = %+v, %v, %v", it, ok, err)
	}
	writeBenchFile(t, "scorecard.jsonl", `{"pkg":"internal/p","what":"BenchmarkA regressed +10.00%","verdict":"kept"}`)
	if it, ok, err := NextCandidate(now); err != nil || ok {
		t.Fatalf("closed item still queued: %+v, %v, %v", it, ok, err)
	}
}

func writeBenchFile(t *testing.T, name, line string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(benchRoot(), name), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
