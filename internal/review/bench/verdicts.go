package bench

import (
	"cmp"
	"slices"
	"time"
)

func evaluateHistory(history []observation, now time.Time) []Verdict {
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
	verdicts := make([]Verdict, 0, len(groups))
	for _, group := range groups {
		last := group[len(group)-1]
		verdict, reason := benchmarkVerdict(group, now)
		verdicts = append(verdicts, Verdict{Pkg: last.Pkg, Benchmark: last.Benchmark, Epoch: last.Epoch,
			Verdict: verdict, Reason: reason})
	}
	slices.SortFunc(verdicts, func(a, b Verdict) int {
		return cmp.Compare(a.Pkg+"\x00"+a.Benchmark, b.Pkg+"\x00"+b.Benchmark)
	})
	return verdicts
}

type verdictEvidence struct {
	latest             observation
	compared, decisive time.Time
	path, subject      string
}

func summarizeEvidence(history []observation) verdictEvidence {
	var e verdictEvidence
	for _, row := range history {
		if row.Score != nil {
			e.latest = row
		}
		if row.Compared {
			e.compared = row.At
		}
		if row.Decisive {
			e.decisive = row.At
		}
		if row.ProductionPath != "" {
			e.path = row.ProductionPath
		}
		if row.Subject != "" {
			e.subject = row.Subject
		}
	}
	return e
}

func benchmarkVerdict(history []observation, now time.Time) (string, string) {
	e := summarizeEvidence(history)
	switch {
	case e.subject == "gone":
		return "dead", "recorded subject is gone; owner approval required for deletion"
	case e.path == "off":
		return "off-path", "recorded evidence says production does not run the subject"
	}
	if verdict, reason := measurementVerdict(history, e.latest); verdict != "" {
		return verdict, reason
	}
	switch {
	case now.Sub(history[0].At) >= idleAge && (e.compared.IsZero() || now.Sub(e.decisive) >= idleAge):
		return "idle", "never compared or no decisive evidence in 30 days"
	case e.path != "on":
		return "idle", "production path is unverified"
	case e.compared.IsZero() && e.decisive.IsZero():
		return "idle", "no comparison or decision evidence"
	}
	return "working", "stable, on production path, and compared or decisive"
}

func measurementVerdict(history []observation, latest observation) (string, string) {
	switch {
	case latest.Score == nil:
		return "idle", "no measured run in this epoch"
	case !isQuiet(*latest.Score) || latest.Score.Metrics["ns/op"].CV > noiseFlagCV || crossRunCV(history, latest) > noiseFlagCV:
		return "noisy", "load or within/across-run CV exceeds the quiet threshold"
	case latest.Score.Samples > 0 && latest.Score.CostSec/float64(latest.Score.Samples) > slowSampleSec:
		return "costly", "measured cost exceeds two seconds per sample"
	case latest.Score.Samples < 2:
		return "idle", "insufficient samples"
	}
	return "", ""
}

func crossRunCV(history []observation, latest observation) float64 {
	medians := make([]float64, 0, quietWindow)
	for i := len(history) - 1; i >= 0 && len(medians) < quietWindow; i-- {
		row := history[i]
		if row.Score != nil && isQuiet(*row.Score) && sameHost(row.Env, latest.Env) {
			if metric, ok := row.Score.Metrics["ns/op"]; ok {
				medians = append(medians, metric.Median)
			}
		}
	}
	if len(medians) < 2 {
		return 0
	}
	return spread(medians).CV
}

func sameHost(a, b Env) bool {
	return a.Machine == b.Machine && a.GOOS == b.GOOS && a.GOARCH == b.GOARCH && a.Go == b.Go &&
		a.CPUs == b.CPUs && a.GOMAXPROCS == b.GOMAXPROCS
}
