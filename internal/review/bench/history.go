package bench

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

const (
	historySchema = 1
	quietWindow   = 5
	idleAge       = 30 * 24 * time.Hour
)

type observation struct {
	Schema         int       `json:"schema"`
	At             time.Time `json:"at"`
	Pkg            string    `json:"pkg"`
	Benchmark      string    `json:"benchmark"`
	Epoch          string    `json:"epoch"`
	Env            Env       `json:"env"`
	Score          *Score    `json:"score,omitempty"`
	Compared       bool      `json:"compared,omitempty"`
	Decisive       bool      `json:"decisive,omitempty"`
	ProductionPath string    `json:"production_path,omitempty"`
	Subject        string    `json:"subject,omitempty"`
}

var historyFormat = schemas.Register(schemas.Format{
	Name: "bench-history", Owner: "internal/review/bench", Kind: schemas.Log, Version: historySchema,
	Glob: func() string { return filepath.Join(benchRoot(), "*", "history.jsonl") },
	New:  func() any { return new(observation) },
	Keep: "benchmark evidence and epochs; deletion requires owner approval",
})

// Verdict reports evidence only; no verdict authorizes a deletion.
type Verdict struct {
	Pkg       string `json:"pkg"`
	Benchmark string `json:"benchmark"`
	Epoch     string `json:"epoch"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
}

func readHistory(file string) ([]observation, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []observation
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for line := 1; sc.Scan(); line++ {
		var row observation
		if _, err := schemas.Decode(historyFormat, sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, line, err)
		}
		if row.At.IsZero() || row.Pkg == "" || row.Benchmark == "" || !validEpoch(row.Epoch) {
			return nil, fmt.Errorf("%s:%d: incomplete benchmark evidence", file, line)
		}
		out = append(out, row)
	}
	return out, sc.Err()
}

func appendHistory(dir string, rows []observation) error {
	var data strings.Builder
	enc := json.NewEncoder(&data)
	for _, row := range rows {
		row.Schema = historySchema
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return paths.AppendLine(filepath.Join(dir, "history.jsonl"), []byte(data.String()))
}

func recordRun(dir string, rep Report, res PkgResult, at time.Time) ([]Verdict, error) {
	history, err := readHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		return nil, err
	}
	var rows []observation
	for _, s := range res.Benchmarks {
		if !validEpoch(s.Epoch) {
			return nil, fmt.Errorf("%s: benchmark source was not found", s.Name)
		}
		rows = append(rows, observation{At: at, Pkg: res.Pkg, Benchmark: s.Name, Epoch: s.Epoch,
			Env: rep.Env, Score: &s, Compared: len(s.Deltas) > 0, Decisive: moved(s) && isQuiet(s)})
	}
	if err := recordEpochChanges(history, rows, rep.Commit); err != nil {
		return nil, err
	}
	if err := appendHistory(dir, rows); err != nil {
		return nil, err
	}
	return evaluateHistory(slices.Concat(history, rows), at), nil
}

func recordEpochChanges(history, rows []observation, sha string) error {
	last := map[string]string{}
	files := map[string]string{}
	for _, row := range history {
		last[row.Benchmark] = row.Epoch
		if row.Score != nil {
			files[row.Score.Source] = row.Epoch
		}
	}
	for _, row := range rows {
		old := last[row.Benchmark]
		if old == "" && row.Score != nil {
			old = files[row.Score.Source]
		}
		if old == "" || old == row.Epoch {
			continue
		}
		data, err := json.Marshal(Row{At: row.At, Pkg: row.Pkg, Benchmark: row.Benchmark, Epoch: row.Epoch,
			OldEpoch: old, Verdict: "bench-change", What: "benchmark file changed", Sha: sha})
		if err != nil {
			return err
		}
		if err := paths.AppendLine(filepath.Join(benchRoot(), "scorecard.jsonl"), data); err != nil {
			return err
		}
	}
	return nil
}

func validateRecord(l loopOpts) error {
	if l.pkg == "" || l.what == "" || !slices.Contains([]string{"kept", "reverted", "bench-change"}, l.verdict) {
		return errors.New("--record needs --pkg, --what and --verdict kept|reverted|bench-change")
	}
	if !filepath.IsLocal(l.pkg) || strings.Contains(l.pkg, "\\") {
		return errors.New("--pkg must be a local package path")
	}
	return nil
}

func validateEvidence(l loopOpts) error {
	if err := validateRecord(l); err != nil {
		return err
	}
	if !slices.Contains([]string{"", "on", "off"}, l.productionPath) ||
		!slices.Contains([]string{"", "present", "gone"}, l.subject) {
		return errors.New("--production-path must be on|off; --subject must be present|gone")
	}
	if l.benchmark+l.epoch+l.oldEpoch+l.productionPath+l.subject == "" &&
		l.verdict != "bench-change" {
		return nil
	}
	if !strings.HasPrefix(l.benchmark, "Benchmark") || !validEpoch(l.epoch) {
		return errors.New("benchmark evidence needs --benchmark and a lowercase SHA-256 --epoch")
	}
	return validateEpochChange(l)
}

func validateEpochChange(l loopOpts) error {
	if l.verdict == "bench-change" && (!validEpoch(l.oldEpoch) || l.oldEpoch == l.epoch) {
		return errors.New("bench-change needs distinct --old-epoch and --epoch hashes")
	}
	if l.oldEpoch != "" && l.verdict != "bench-change" {
		return errors.New("--old-epoch requires --verdict bench-change")
	}
	return nil
}

func recordEvidence(l loopOpts, at time.Time) error {
	if l.benchmark == "" {
		return nil
	}
	return appendHistory(filepath.Join(benchRoot(), pkgDirName(l.pkg)), []observation{{
		At: at, Pkg: l.pkg, Benchmark: l.benchmark, Epoch: l.epoch,
		Decisive:       l.verdict == "kept" || l.verdict == "reverted",
		ProductionPath: l.productionPath, Subject: l.subject,
	}})
}

func runVerdicts(l loopOpts, asJSON bool, w io.Writer) error {
	files, err := filepath.Glob(filepath.Join(benchRoot(), "*", "history.jsonl"))
	if err != nil {
		return err
	}
	var rows []observation
	for _, file := range files {
		history, err := readHistory(file)
		if err != nil {
			return err
		}
		for _, row := range history {
			if l.pkg == "" || row.Pkg == l.pkg {
				rows = append(rows, row)
			}
		}
	}
	verdicts := evaluateHistory(rows, time.Now().UTC())
	if asJSON {
		return json.NewEncoder(w).Encode(verdicts)
	}
	for _, v := range verdicts {
		fmt.Fprintf(w, "%s %s: %s (%s; gates only)\n", v.Pkg, v.Benchmark, v.Verdict, v.Reason)
	}
	return nil
}
