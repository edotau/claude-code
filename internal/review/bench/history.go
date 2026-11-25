package bench

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"maps"
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
	Repo           string    `json:"repo,omitempty"`
}

// repoIdentity is the repository top-level path, symlink-resolved and cleaned; unresolvable stays as given, cleaned.
func repoIdentity(top string) string {
	if top == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(top); err == nil {
		top = real
	}
	return filepath.Clean(top)
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

// recordRun holds history.lock (not AppendLine's history.jsonl.lock) so concurrent runs see each other's rows.
func recordRun(dir string, rep Report, res PkgResult, at time.Time) (verdicts []Verdict, err error) {
	err = paths.WithFileLock(filepath.Join(dir, "history"), func() error {
		verdicts, err = recordRunLocked(dir, rep, res, at)
		return err
	})
	return verdicts, err
}

func recordRunLocked(dir string, rep Report, res PkgResult, at time.Time) ([]Verdict, error) {
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
			Env: rep.Env, Score: &s, Compared: len(s.Deltas) > 0, Decisive: moved(s) && isQuiet(s),
			Repo: repoIdentity(cmp.Or(res.repo, res.top))})
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
		data, err := json.Marshal(scorecardRow{At: row.At, Pkg: row.Pkg, Benchmark: row.Benchmark, Epoch: row.Epoch,
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

type benchmarkEpoch struct {
	File string `json:"file"`
	Hash string `json:"hash"`
}

const epochPrefix = "# bench-epochs: "

func benchmarkFunc(name string) string {
	root, _, _ := strings.Cut(name, "/")
	return procSuffix.ReplaceAllString(root, "")
}

func benchmarkEpochs(top, pkg string) (map[string]benchmarkEpoch, error) {
	dir := filepath.Join(top, pkg)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]benchmarkEpoch{}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		epochs, err := fileEpochs(top, filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, err
		}
		maps.Copy(out, epochs)
	}
	return out, nil
}

func fileEpochs(top, file string) (map[string]benchmarkEpoch, error) {
	active, err := build.Default.MatchFile(filepath.Dir(file), filepath.Base(file))
	if err != nil || !active {
		return nil, err
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, body, 0)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(top, file)
	if err != nil {
		return nil, err
	}
	// CRLF folds to LF so an autocrlf checkout keeps the epoch.
	hash := sha256.Sum256(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")))
	out := map[string]benchmarkEpoch{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Benchmark") {
			out[fn.Name.Name] = benchmarkEpoch{File: filepath.ToSlash(rel), Hash: hex.EncodeToString(hash[:])}
		}
	}
	return out, nil
}

func epochHeader(epochs map[string]benchmarkEpoch) string {
	data, _ := json.Marshal(epochs) // strings only
	return epochPrefix + string(data) + "\n"
}

func parseEpochs(text string) (map[string]benchmarkEpoch, error) {
	for line := range strings.SplitSeq(text, "\n") {
		if raw, ok := strings.CutPrefix(line, epochPrefix); ok {
			var epochs map[string]benchmarkEpoch
			if err := json.Unmarshal([]byte(raw), &epochs); err != nil || epochs == nil {
				return nil, fmt.Errorf("invalid benchmark epoch header: %s", line)
			}
			for name, epoch := range epochs {
				if name == "" || epoch.File == "" || !validEpoch(epoch.Hash) {
					return nil, fmt.Errorf("invalid benchmark epoch for %q", name)
				}
			}
			return epochs, nil
		}
	}
	return nil, nil
}

func sameEpochs(before, after string) bool {
	b, berr := parseEpochs(before)
	a, aerr := parseEpochs(after)
	return berr == nil && aerr == nil && maps.Equal(b, a) && (b == nil) == (a == nil)
}

func validEpoch(epoch string) bool {
	decoded, err := hex.DecodeString(epoch)
	return err == nil && len(decoded) == sha256.Size && epoch == strings.ToLower(epoch)
}

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
	case e.compared.IsZero() && e.decisive.IsZero():
		return "idle", "no comparison or decision evidence"
	case e.path != "on": // design §10: idle means never compared or decisive, not an unrecorded production path
		return "unverified", "compared, but no production-path record (--production-path on|off)"
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
