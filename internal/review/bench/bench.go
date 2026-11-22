// Package bench is `review bench`: run a package's benchmarks, store them, compare with the rolling baseline.
package bench

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const (
	noiseFlagCV   = 5.0 // ns/op coefficient of variation, percent; unlike the range it does not grow with --count
	slowSampleSec = 2.0 // measured seconds per sample; go aims each at -benchtime (1 s), so 2x means one op runs long
	hundred       = 100
	runTimeout    = 30 * time.Minute
	waitDelay     = 10 * time.Second
	rollingLock   = 5 * time.Second
)

type options struct {
	paths   []string
	count   int
	re      string
	base    bool
	out     string
	verbose bool
	json    bool
	loop    loopOpts
}

// Delta is one benchstat comparison cell for a benchmark and unit.
type Delta struct {
	Old         string `json:"old"`
	New         string `json:"new"`
	Change      string `json:"change"`
	P           string `json:"p"`
	Significant bool   `json:"significant"`
}

// Spread is one metric's samples reduced: range, median and coefficient of variation in percent.
type Spread struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
	CV     float64 `json:"cv_pct"`
}

// Score is one benchmark: every reported metric's spread, noise, own run cost and the delta vs rolling.
type Score struct {
	Name     string            `json:"name"`
	Samples  int               `json:"samples"`
	Metrics  map[string]Spread `json:"metrics"`
	NoisePct float64           `json:"noise_pct"`
	CostSec  float64           `json:"cost_sec"`
	Flags    []string          `json:"flags,omitempty"`
	Deltas   map[string]Delta  `json:"deltas,omitempty"`
	Epoch    string            `json:"epoch,omitempty"`
	Source   string            `json:"source,omitempty"`
}

// PkgResult is one package's run: where it was stored, what the process cost and how each benchmark scored.
type PkgResult struct {
	Pkg        string    `json:"pkg"`
	File       string    `json:"file"`
	Compared   bool      `json:"compared"`
	Baseline   bool      `json:"baseline,omitempty"` // this run was written as rolling.txt
	Stats      RunStats  `json:"stats"`
	Benchmarks []Score   `json:"benchmarks"`
	Missing    []string  `json:"missing,omitempty"`
	Verdicts   []Verdict `json:"verdicts,omitempty"`
}

// Report is the whole run, the input of the improvement loop.
type Report struct {
	Env
	Bench    string      `json:"bench"`
	Count    int         `json:"count"`
	Mode     string      `json:"mode"`
	NoisyRun bool        `json:"noisy_load"`
	Packages []PkgResult `json:"packages"`
}

type sample struct {
	iters float64
	vals  map[string]float64
}

// RunBench is `claude-code review bench`: 0 even on regressions, 1 on a run error, 2 on usage.
func RunBench(args []string, stdout, stderr io.Writer) int {
	o, code, done := parseArgs(args, stdout, stderr)
	if done {
		return code
	}
	if o.loop.queue || o.loop.record || o.loop.verdicts {
		return runLoop(o, stdout, stderr)
	}
	if _, err := exec.LookPath("benchstat"); err != nil {
		fmt.Fprintln(stderr, "review bench: benchstat not on PATH (go install golang.org/x/perf/cmd/benchstat@latest)")
		return 1
	}
	if o.loop.decide {
		return runLoop(o, stdout, stderr)
	}
	rep, err := run(o, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "review bench:", err)
		return 1
	}
	if o.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	printText(stdout, rep, o.verbose)
	return 0
}

func parseArgs(args []string, stdout, stderr io.Writer) (options, int, bool) {
	var o options
	var format string
	fs := helptext.NewFlagSet("review bench", helptext.Page{
		Usage: "claude-code review bench [flags]",
		About: "Run the benchmarks of changed packages and compare them with the rolling baseline.",
	})
	fs.Func("path", "file or dir of a package to run, repeatable; default: changed", func(s string) error {
		o.paths = append(o.paths, s)
		return nil
	})
	fs.IntVar(&o.count, "count", 10, "samples per benchmark")
	fs.StringVar(&o.re, "bench", ".", "benchmark name regexp")
	fs.BoolVar(&o.base, "base", false, "store the run as a base and as rolling.txt")
	fs.StringVar(&o.out, "out", "", "also write the run text to this file (one package)")
	fs.BoolVar(&o.verbose, "v", false, "also show benchmarks that did not move")
	fs.StringVar(&format, "format", "text", "text|json")
	loopFlags(fs, &o.loop)
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return o, 0, true
	}
	fs.SetOutput(stderr)
	if _, err := helptext.ParseArgs(fs, args); err != nil {
		return o, helptext.ExitCode(err), true
	}
	if format != "text" && format != "json" || o.count < 1 {
		fmt.Fprintln(stderr, "review bench: --format must be text|json and --count at least 1")
		return o, 2, true
	}
	o.json = format == "json"
	if msg := o.flagConflict(); msg != "" {
		fmt.Fprintln(stderr, "review bench:", msg)
		return o, 2, true
	}
	return o, 0, false
}

func (o options) flagConflict() string {
	l := o.loop
	switch {
	case l.kind != "" && !l.queue:
		return "--kind needs --queue"
	case l.kind != "" && l.kind != "fix" && l.kind != "weak":
		return "--kind must be fix|weak"
	case o.out != "" && (l.queue || l.decide || l.record || l.verdicts):
		return "--out only applies to a benchmark run"
	}
	return ""
}

func run(o options, stderr io.Writer) (Report, error) {
	top, err := repoTop()
	if err != nil {
		return Report{}, err
	}
	pkgs, err := benchPackages(top, o.paths)
	if err != nil {
		return Report{}, err
	}
	if o.out != "" && len(pkgs) != 1 {
		return Report{}, fmt.Errorf("--out needs exactly one benchmark package, got %d", len(pkgs))
	}
	rep := header(top, o)
	for _, p := range pkgs {
		fmt.Fprintf(stderr, "review bench: %s\n", p)
		res, err := runPackage(top, p, o, rep)
		if err != nil {
			return rep, err
		}
		rep.Packages = append(rep.Packages, res)
		if err := writeOut(o.out, res.File); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// writeOut copies the stored run text to out, atomically; "" is no copy.
func writeOut(out, runFile string) error {
	if out == "" {
		return nil
	}
	b, err := os.ReadFile(runFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return paths.AtomicWriteNoSync(out, b, 0o644)
}

func repoTop() (string, error) {
	out, err := source.GitInherit(".", "rev-parse", "--show-toplevel")
	if err != nil || out == "" {
		return "", errors.New("not a git repository")
	}
	return out, nil
}

func header(top string, o options) Report {
	env := collectEnv(top)
	mode := "run"
	if o.base {
		mode = "base"
	}
	return Report{
		Env: env, Bench: o.re, Count: o.count, Mode: mode,
		NoisyRun: env.Load1 > float64(runtime.NumCPU())/2,
	}
}

// benchPackages maps --path files (else the changed files) to package dirs that hold a Benchmark func.
func benchPackages(top string, targets []string) ([]string, error) {
	if len(targets) == 0 {
		var err error
		if targets, err = changedFiles(top); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		abs, _ := filepath.Abs(t)
		dir := abs
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			dir = filepath.Dir(abs)
		}
		rel, err := filepath.Rel(top, dir)
		if err != nil || strings.HasPrefix(rel, "..") || seen[rel] || !hasBenchmarks(dir) {
			continue
		}
		seen[rel] = true
		out = append(out, filepath.ToSlash(rel))
	}
	slices.Sort(out)
	return out, nil
}

func changedFiles(top string) ([]string, error) {
	names, err := source.GitInherit(top, "diff", "--name-only", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	untracked, _ := source.GitInherit(top, "ls-files", "--others", "--exclude-standard")
	var out []string
	for n := range strings.SplitSeq(names+"\n"+untracked, "\n") {
		if n != "" {
			out = append(out, filepath.Join(top, n))
		}
	}
	return out, nil
}

var benchFunc = regexp.MustCompile(`(?m)^func Benchmark`)

func hasBenchmarks(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && benchFunc.Match(b) {
			return true
		}
	}
	return false
}

func goTest(top, pkg string, o options) (out string, stats RunStats, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-trimpath", "-run", "^$", "-bench", o.re, "-benchmem", // -trimpath: snapshot dirs share cache entries
		"-count", strconv.Itoa(o.count), "./"+pkg)
	cmd.Dir = top
	ownGroup(cmd)
	cmd.WaitDelay = waitDelay
	var outb, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &errb
	before, start := loadAverage(), time.Now()
	runErr := cmd.Run()
	stats = statsOf(cmd, time.Since(start), before)
	if runErr != nil {
		return "", stats, fmt.Errorf("go test %s: %w\n%s%s", pkg, runErr, outb.String(), errb.String())
	}
	return outb.String(), stats, nil
}

func runPackage(top, pkg string, o options, rep Report) (PkgResult, error) {
	dir := filepath.Join(benchRoot(), pkgDirName(pkg))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PkgResult{}, err
	}
	epochs, err := benchmarkEpochs(top, pkg)
	if err != nil {
		return PkgResult{}, err
	}
	out, stats, err := goTest(top, pkg, o)
	if err != nil {
		return PkgResult{}, err
	}
	current, err := benchmarkEpochs(top, pkg)
	if err != nil {
		return PkgResult{}, err
	}
	if epochHeader(current) != epochHeader(epochs) {
		return PkgResult{}, errors.New("benchmark source changed during measurement; rerun")
	}
	head := fmt.Sprintf("%spackage: %s\nbench: %s\ncount: %d\n%s", rep.lines(), pkg, o.re, o.count, stats.lines())
	// "# " keeps run metadata out of benchstat's config keys: a differing key (commit, load) splits files into tables
	body := "# " + strings.ReplaceAll(strings.TrimSuffix(head, "\n"), "\n", "\n# ") + "\n" + out
	body = epochHeader(epochs) + body
	file, err := writeRunFile(dir, time.Now(), rep, body)
	if err != nil {
		return PkgResult{}, err
	}
	noisy := rep.NoisyRun || max(stats.LoadBefore, stats.LoadAfter) > float64(rep.CPUs)/2
	res := PkgResult{Pkg: pkg, File: file, Stats: stats, Benchmarks: scoreAll(out, noisy)}
	for i := range res.Benchmarks {
		e := epochs[benchmarkFunc(res.Benchmarks[i].Name)]
		res.Benchmarks[i].Epoch, res.Benchmarks[i].Source = e.Hash, e.File
	}
	if err := compareRolling(&res, dir, body, o.base, !rep.Dirty); err != nil {
		return res, err
	}
	res.Verdicts, err = recordRun(dir, rep, res, time.Now().UTC())
	return res, err
}

// compareRolling compares res with dir's rolling.txt; with none and seed (a clean tree), the run becomes it.
func compareRolling(res *PkgResult, dir, body string, base, seed bool) error {
	rolling := filepath.Join(dir, "rolling.txt")
	old, err := os.ReadFile(rolling)
	switch {
	case err == nil:
		if !sameEpochs(string(old), body) {
			res.Missing = append(res.Missing, "benchmark epoch changed; this run starts a new package baseline")
			base = true
			break
		}
		deltas, err := compare(rolling, res.File)
		if err != nil {
			return err
		}
		res.Compared = true
		attach(res.Benchmarks, deltas)
		res.Missing = missingBench(string(old), body)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if base || seed && err != nil {
		res.Baseline = true
		return writeRolling(dir, []byte(body))
	}
	return nil
}

// writeRunFile creates the run file exclusively; the time and pid keep two runs from overwriting each other.
func writeRunFile(dir string, at time.Time, rep Report, body string) (string, error) {
	name := fmt.Sprintf("%s-%s-%s-%d.txt", at.Format("2006-01-02-150405"), rep.Commit, rep.Mode, os.Getpid())
	file := filepath.Join(dir, name)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(body)
	return file, errors.Join(werr, f.Close())
}

// benchUnits lists "benchmark unit" for every metric a run text holds.
func benchUnits(text string) map[string]bool {
	_, by := parseSamples(text)
	out := map[string]bool{}
	for n, ss := range by {
		for _, s := range ss {
			for u := range s.vals {
				out[n+" "+u] = true
			}
		}
	}
	return out
}

// missingBench names the benchmark/unit pairs that only one of the two run texts holds.
func missingBench(before, after string) []string {
	b, a := benchUnits(before), benchUnits(after)
	var out []string
	for k := range b {
		if !a[k] {
			out = append(out, "missing after: "+k)
		}
	}
	for k := range a {
		if !b[k] {
			out = append(out, "missing before: "+k)
		}
	}
	slices.Sort(out)
	return out
}

func compare(old, cur string) (map[string]map[string]Delta, error) {
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "benchstat", "-format", "csv", old, cur)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("benchstat: %w: %s", err, errb.String())
	}
	return parseBenchstatCSV(out.String()), nil
}

func attach(scores []Score, deltas map[string]map[string]Delta) {
	for i := range scores {
		scores[i].Deltas = deltas[strings.TrimPrefix(scores[i].Name, "Benchmark")] // benchstat drops the prefix
	}
}

// parseBenchstatCSV reads `benchstat -format csv` into benchmark -> unit -> Delta.
func parseBenchstatCSV(text string) map[string]map[string]Delta {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	rows, _ := r.ReadAll()
	out := map[string]map[string]Delta{}
	unit := ""
	for _, row := range rows {
		switch {
		case len(row) >= 6 && row[0] == "" && row[5] == "vs base":
			unit = row[1]
		case unit != "" && len(row) >= 7 && row[0] != "" && row[0] != "geomean":
			if out[row[0]] == nil {
				out[row[0]] = map[string]Delta{}
			}
			out[row[0]][unit] = Delta{Old: row[1], New: row[3], Change: row[5], P: row[6], Significant: row[5] != "~"}
		}
	}
	return out
}

// parseSamples groups `go test -bench` result lines by benchmark; every "value unit" pair is a metric.
func parseSamples(text string) (names []string, by map[string][]sample) {
	by = map[string][]sample{}
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		s := sample{vals: map[string]float64{}}
		s.iters, _ = strconv.ParseFloat(f[1], 64)
		for i := 3; i < len(f); i += 2 {
			if v, err := strconv.ParseFloat(f[i-1], 64); err == nil {
				s.vals[f[i]] = v
			}
		}
		if _, ok := by[f[0]]; !ok {
			names = append(names, f[0])
		}
		by[f[0]] = append(by[f[0]], s)
	}
	return names, by
}

func scoreAll(text string, noisyLoad bool) []Score {
	names, by := parseSamples(text)
	out := make([]Score, 0, len(names))
	for _, n := range names {
		out = append(out, score(n, by[n], noisyLoad))
	}
	return out
}

func score(name string, ss []sample, noisyLoad bool) Score {
	sc := Score{Name: name, Samples: len(ss), Metrics: map[string]Spread{}}
	byUnit := map[string][]float64{}
	for _, s := range ss {
		for u, v := range s.vals {
			byUnit[u] = append(byUnit[u], v)
		}
		sc.CostSec += s.iters * s.vals["ns/op"] / 1e9
	}
	for u, v := range byUnit {
		sc.Metrics[u] = spread(v)
	}
	if ns := sc.Metrics["ns/op"]; ns.Min > 0 {
		sc.NoisePct = round((ns.Max - ns.Min) / ns.Min * hundred)
	}
	sc.CostSec = round(sc.CostSec)
	if sc.Metrics["ns/op"].CV > noiseFlagCV {
		sc.Flags = append(sc.Flags, "noisy")
	}
	if sc.CostSec/float64(len(ss)) > slowSampleSec {
		sc.Flags = append(sc.Flags, "slow")
	}
	if noisyLoad {
		sc.Flags = append(sc.Flags, "load")
	}
	return sc
}

func spread(v []float64) Spread {
	sp := Spread{Min: slices.Min(v), Median: median(v), Max: slices.Max(v)}
	if len(v) < 2 {
		return sp
	}
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	ss := 0.0
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	if mean != 0 {
		sp.CV = round(math.Sqrt(ss/float64(len(v)-1)) / mean * hundred)
	}
	return sp
}

func median(v []float64) float64 {
	s := slices.Sorted(slices.Values(v))
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func round(v float64) float64 { return math.Round(v*hundred) / hundred }

func moved(s Score) bool {
	for _, d := range s.Deltas {
		if d.Significant {
			return true
		}
	}
	return false
}

func printText(w io.Writer, r Report, verbose bool) {
	noisy := ""
	if r.NoisyRun {
		noisy = " (noisy)"
	}
	fmt.Fprintf(w, "commit %s dirty=%t  %s  load %.2f/%d cpus%s  count %d\n",
		r.Commit, r.Dirty, r.Go, r.Load1, r.CPUs, noisy, r.Count)
	if len(r.Packages) == 0 {
		fmt.Fprintln(w, "no changed package holds benchmarks")
	}
	for _, p := range r.Packages {
		st := p.Stats
		fmt.Fprintf(w, "\n%s  -> %s\n  wall %.1fs user %.1fs sys %.1fs rss %dMB load %.2f->%.2f\n",
			p.Pkg, p.File, st.WallSec, st.UserSec, st.SysSec, st.MaxRSSKB/kbPerMB, st.LoadBefore, st.LoadAfter)
		switch {
		case !p.Compared && p.Baseline:
			fmt.Fprintln(w, "  no rolling.txt baseline; this run is now the baseline")
		case !p.Compared:
			fmt.Fprintln(w, "  no rolling.txt baseline; run with --base (or on a clean tree) to create one")
		}
		for _, m := range p.Missing {
			fmt.Fprintln(w, "  "+m)
		}
		for _, s := range p.Benchmarks {
			printScore(w, s, verbose)
		}
		for _, v := range p.Verdicts {
			fmt.Fprintf(w, "  %s: %s (%s; gates only)\n", v.Benchmark, v.Verdict, v.Reason)
		}
	}
}

func printScore(w io.Writer, s Score, verbose bool) {
	if !verbose && !moved(s) && len(s.Flags) == 0 {
		return
	}
	m := s.Metrics
	fmt.Fprintf(w, "  %-44s %10.1f ns/op %8.0f B/op %5.0f allocs/op  noise %.1f%% cv %.1f%%  cost %.1fs",
		s.Name, m["ns/op"].Median, m["B/op"].Median, m["allocs/op"].Median, s.NoisePct, m["ns/op"].CV, s.CostSec)
	if len(s.Flags) > 0 {
		fmt.Fprintf(w, "  [%s]", strings.Join(s.Flags, ","))
	}
	fmt.Fprintln(w)
	for _, u := range []string{"sec/op", "B/op", "allocs/op"} {
		if d, ok := s.Deltas[u]; ok && (verbose || d.Significant) {
			fmt.Fprintf(w, "      %-10s %s -> %s  %s  (%s)\n", u, short(d.Old), short(d.New), d.Change, d.P)
		}
	}
}

// short prints a benchstat CSV number in 4 significant digits; anything that is not a number stays as written.
func short(v string) string {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return v
	}
	return strconv.FormatFloat(f, 'g', 4, 64)
}
