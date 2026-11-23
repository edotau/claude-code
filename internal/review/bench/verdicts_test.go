package bench

import (
	"strings"
	"testing"
	"time"
)

func TestBenchmarkVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := observation{At: now, Pkg: "p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64),
		Compared: true, ProductionPath: "on", Subject: "present",
		Score: &Score{Samples: 2, CostSec: 2, Metrics: map[string]Spread{"ns/op": {Median: 100, CV: 1}}}}
	cases := []struct {
		name, want string
		change     func(*observation)
	}{
		{"stable compared", "working", func(*observation) {}},
		{"gone", "dead", func(r *observation) { r.Subject = "gone" }},
		{"off path", "off-path", func(r *observation) { r.ProductionPath = "off" }},
		{"unverified", "unverified", func(r *observation) { r.ProductionPath = "" }},
		{"never compared", "idle", func(r *observation) { r.ProductionPath, r.Compared = "", false }},
		{"no samples", "idle", func(r *observation) { r.Score = nil }},
		{"within run noise", "noisy", func(r *observation) { r.Score.Metrics = map[string]Spread{"ns/op": {CV: 6}} }},
		{"loaded", "noisy", func(r *observation) { r.Score.Flags = []string{"load"} }},
		{"expensive sample", "costly", func(r *observation) { r.Score.CostSec = 5 }},
		{"never decisive", "idle", func(r *observation) { r.At = now.Add(-idleAge) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, score := base, *base.Score
			row.Score = &score
			tc.change(&row)
			got := evaluateHistory([]observation{row}, now)
			if len(got) != 1 || got[0].Verdict != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestVerdictsResetHistoryAtEpochAndIgnoreOtherHosts(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	old := observation{At: now.Add(-time.Hour), Pkg: "p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64),
		ProductionPath: "on", Compared: true,
		Score: &Score{Samples: 2, Metrics: map[string]Spread{"ns/op": {Median: 100}}}}
	newer := old
	newer.At, newer.ProductionPath = now, ""
	newer.Score = &Score{Samples: 2, Metrics: map[string]Spread{"ns/op": {Median: 200}}}
	if got := evaluateHistory([]observation{newer, old}, now); got[0].Verdict != "noisy" {
		t.Fatalf("cross-run CV: %+v", got)
	}
	newer.Env.Machine = "other host"
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "working" {
		t.Fatalf("host isolation: %+v", got)
	}
	newer.Epoch = strings.Repeat("b", 64)
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "unverified" {
		t.Fatalf("epoch leaked path evidence: %+v", got)
	}
	newer.ProductionPath, newer.Decisive = "on", true
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "working" {
		t.Fatalf("new baseline: %+v", got)
	}
}
