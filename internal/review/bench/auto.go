package bench

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"time"
)

// historyCandidates derives fix and benchmark items from each benchmark's latest epoch; noisy runs alone queue nothing.
func historyCandidates(history []observation) []Item {
	ordered := slices.Clone(history)
	slices.SortStableFunc(ordered, func(a, b observation) int { return a.At.Compare(b.At) })
	groups := map[string][]observation{}
	for _, row := range ordered {
		key := row.Pkg + "\x00" + row.Benchmark
		group := groups[key]
		if len(group) > 0 && group[len(group)-1].Epoch != row.Epoch {
			group = nil
		}
		groups[key] = append(group, row)
	}
	var out []Item
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		if it, ok := groupCandidate(groups[key]); ok {
			out = append(out, it)
		}
	}
	return out
}

// usable rejects an empty or load-held run, which must never queue a candidate.
func usable(row observation) bool {
	return row.Score != nil && row.Score.Samples >= 2 && !slices.Contains(row.Score.Flags, "load")
}

func groupCandidate(group []observation) (Item, bool) {
	var measured []int
	for i, row := range group {
		if row.Score != nil {
			measured = append(measured, i)
		}
	}
	if len(measured) == 0 || !usable(group[measured[len(measured)-1]]) {
		return Item{}, false
	}
	at := measured[len(measured)-1]
	last := group[at]
	name := procSuffix.ReplaceAllString(last.Benchmark, "")
	it := Item{Pkg: last.Pkg, Source: "history"}
	switch verdict, _ := measurementVerdict(group[:at+1], last); verdict {
	case "costly":
		it.Kind = "fix"
		it.What = fmt.Sprintf("%s is costly: %.2f s per sample", name, last.Score.CostSec/float64(last.Score.Samples))
		return it, true
	case "":
		return regression(it, name, *last.Score)
	case "noisy":
		return noisyTwice(it, name, group, measured)
	}
	return Item{}, false
}

// regression queues a clean run's significant sec/op rise that the decide rule would count.
func regression(it Item, name string, s Score) (Item, bool) {
	d, ok := s.Deltas["sec/op"]
	if !ok || !d.Significant {
		return Item{}, false
	}
	pct, ok := changePct(d)
	if !ok || pct < minRegressionPct || math.IsInf(pct, 0) {
		return Item{}, false
	}
	it.Kind, it.What = "fix", name+" regressed "+d.Change
	it.EstSavingMS = s.Metrics["ns/op"].Median * pct / 100 / 1e6
	return it, true
}

func noisyTwice(it Item, name string, group []observation, measured []int) (Item, bool) {
	if len(measured) < 2 {
		return Item{}, false
	}
	at := measured[len(measured)-2]
	if !usable(group[at]) {
		return Item{}, false
	}
	if verdict, _ := measurementVerdict(group[:at+1], group[at]); verdict != "noisy" {
		return Item{}, false
	}
	it.Kind, it.What = "benchmark", name+" is noisy twice in a row: rewrite the benchmark"
	return it, true
}

// Candidates ranks the items derived from every package's benchmark history, closed ones dropped.
func Candidates(now time.Time) ([]Item, error) {
	root := benchRoot()
	files, err := filepath.Glob(filepath.Join(root, "*", "history.jsonl"))
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, file := range files {
		history, err := readHistory(file)
		if err != nil {
			return nil, err
		}
		items = append(items, historyCandidates(history)...)
	}
	perDay, err := callsPerDay(filepath.Join(root, "candidates.jsonl"))
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].CallsPerDay = cmp.Or(perDay[items[i].Pkg], 1)
	}
	rows, err := readJSONL[Row](filepath.Join(root, "scorecard.jsonl"))
	if err != nil {
		return nil, err
	}
	return rank(items, rows, now), nil
}

// callsPerDay is each package's largest calls_per_day in the candidate file; it never seeds the file.
func callsPerDay(file string) (map[string]float64, error) {
	rows, err := readJSONL[Item](file)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, r := range rows {
		out[r.Pkg] = max(out[r.Pkg], r.CallsPerDay)
	}
	return out, nil
}

// NextCandidate is the top-ranked history candidate; false when none is open.
func NextCandidate(now time.Time) (Item, bool, error) {
	items, err := Candidates(now)
	if err != nil || len(items) == 0 {
		return Item{}, false, err
	}
	return items[0], true, nil
}
