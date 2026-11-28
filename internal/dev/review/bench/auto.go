package bench

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

const (
	AuditOff  = "off"
	AuditOn   = "on"
	AuditAuto = "auto"
)

type auditState struct {
	Mode      string    `json:"mode"`
	UpdatedAt time.Time `json:"updated_at"`
}

func auditModeFile() string { return filepath.Join(benchRoot(), "audit-mode.json") }

// AuditMode returns the persisted performance audit mode (off, on, or auto), defaulting to auto.
func AuditMode() string {
	for _, p := range []string{auditModeFile(), filepath.Join(paths.StateDir(), "harness", "bench", "audit-mode.json")} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st auditState
		if json.Unmarshal(data, &st) == nil && slices.Contains([]string{AuditOff, AuditOn, AuditAuto}, st.Mode) {
			return st.Mode
		}
	}
	return AuditAuto
}

// SetAuditMode persists mode (off, on, or auto) to audit-mode.json.
func SetAuditMode(mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if !slices.Contains([]string{AuditOff, AuditOn, AuditAuto}, mode) {
		return fmt.Errorf("invalid audit mode %q: want on, auto, or off", mode)
	}
	st := auditState{Mode: mode, UpdatedAt: time.Now().UTC()}
	if err := paths.WriteJSON(auditModeFile(), st); err != nil {
		return err
	}
	return paths.WriteJSON(filepath.Join(paths.StateDir(), "harness", "bench", "audit-mode.json"), st)
}

// Candidate is a recorded performance bottleneck observation for next-round audit planning.
type Candidate struct {
	Area        string    `json:"area,omitempty"`
	Kind        string    `json:"kind,omitempty"`
	Metric      string    `json:"metric,omitempty"`
	ObservedMs  float64   `json:"observed_ms,omitempty"`
	BudgetMs    float64   `json:"budget_ms,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	RecordedAt  time.Time `json:"recorded_at,omitzero"`
	Audited     bool      `json:"audited,omitempty"`
	Pkg         string    `json:"pkg,omitempty"`
	What        string    `json:"what,omitempty"`
	CallsPerDay float64   `json:"calls_per_day,omitempty"`
	EstSavingMS float64   `json:"est_saving_ms,omitempty"`
	Source      string    `json:"source,omitempty"`
}

// RecordCandidate deduplicates c by Area + Kind and writes it to candidates.jsonl.
func RecordCandidate(c Candidate) error {
	c.Area, c.Kind = strings.TrimSpace(c.Area), strings.TrimSpace(c.Kind)
	if c.Area == "" || c.Kind == "" {
		return errors.New("candidate requires area and kind")
	}
	if c.RecordedAt.IsZero() {
		c.RecordedAt = time.Now().UTC()
	}
	file := filepath.Join(benchRoot(), "candidates.jsonl")
	return paths.WithFileLock(file, func() error {
		rows, err := readJSONL[Candidate](file)
		if err != nil {
			return err
		}
		replaced := false
		for i, r := range rows {
			if r.Area == c.Area && r.Kind == c.Kind {
				rows[i], replaced = c, true
				break
			}
		}
		if !replaced {
			rows = append(rows, c)
		}
		return writeCandidates(file, rows)
	})
}

func writeCandidates(file string, rows []Candidate) error {
	var buf strings.Builder
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := paths.AtomicWrite(file, []byte(buf.String()), 0o600); err != nil {
		return err
	}
	alt := filepath.Join(paths.StateDir(), "harness", "bench", "candidates.jsonl")
	return paths.AtomicWrite(alt, []byte(buf.String()), 0o600)
}

// PendingCandidates returns recorded bottleneck candidates that have Area and Kind and are not yet audited.
func PendingCandidates() ([]Candidate, error) {
	for _, p := range []string{filepath.Join(benchRoot(), "candidates.jsonl"), filepath.Join(paths.StateDir(), "harness", "bench", "candidates.jsonl")} {
		rows, err := readJSONL[Candidate](p)
		if err != nil {
			return nil, err
		}
		var out []Candidate
		for _, r := range rows {
			if r.Area != "" && r.Kind != "" && !r.Audited {
				out = append(out, r)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}

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

// candidates ranks open items from benchmark history; keep filters rows (all when nil) before grouping.
func candidates(now time.Time, keep func(observation) bool) ([]Item, error) {
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
		if keep != nil {
			history = slices.DeleteFunc(history, func(o observation) bool { return !keep(o) })
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
	rows, err := readJSONL[scorecardRow](filepath.Join(root, "scorecard.jsonl"))
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

// NextCandidateFor is the first launchable fix at or above the floor, from rows measured in the repository at top.
func NextCandidateFor(top string, now time.Time, skip map[string]bool) (Item, bool, error) {
	id := repoIdentity(top)
	items, err := candidates(now, func(o observation) bool { return o.Repo != "" && o.Repo == id })
	if err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.Kind == "fix" && it.score() >= minScoreMSDay && !skip[closeKey(it.Pkg, it.What)] &&
			filepath.IsLocal(it.Pkg) && hasBenchmarks(filepath.Join(top, it.Pkg)) {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}
