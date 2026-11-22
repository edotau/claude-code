package bench

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/source"
)

// loopOpts are the improvement-loop modes of `review bench`: --queue, --decide and --record.
type loopOpts struct {
	queue, decide, record             bool
	verdicts                          bool
	benchmark, epoch                  string
	oldEpoch, productionPath, subject string
	top                               int
	before, after                     string
	pkg, what, verdict                string
	agent, model, sha                 string
	dispatch                          string
	tokensIn, tokensOut               int
	tokensCache, attempts             int
	durationS, savingMSDay            float64
}

func loopFlags(fs *flag.FlagSet, l *loopOpts) {
	fs.BoolVar(&l.verdicts, "verdicts", false, "report benchmark verdicts from history; never delete")
	fs.StringVar(&l.benchmark, "benchmark", "", "exact benchmark name for a scorecard row")
	fs.StringVar(&l.epoch, "epoch", "", "benchmark file SHA-256 for --benchmark")
	fs.StringVar(&l.oldEpoch, "old-epoch", "", "previous file SHA-256 for bench-change")
	fs.StringVar(&l.productionPath, "production-path", "", "record observed production path: on|off")
	fs.StringVar(&l.subject, "subject", "", "record observed subject state: present|gone")
	fs.BoolVar(&l.queue, "queue", false, "rank the next fix candidates from the queue, results and seeds")
	fs.BoolVar(&l.decide, "decide", false, "--before vs --after: KEEP 0, REVERT 1, no overlap 3")
	fs.BoolVar(&l.record, "record", false, "add a scorecard row; a kept --after becomes rolling.txt")
	fs.IntVar(&l.top, "top", 5, "queue items to print")
	fs.StringVar(&l.before, "before", "", "benchmark file before the fix")
	fs.StringVar(&l.after, "after", "", "benchmark file after the fix")
	fs.StringVar(&l.pkg, "pkg", "", "package dir, e.g. internal/hooks/safety")
	fs.StringVar(&l.what, "what", "", "the candidate the fix addressed")
	fs.StringVar(&l.verdict, "verdict", "", "kept|reverted|bench-change")
	fs.StringVar(&l.agent, "agent", "", "fix leaf agent")
	fs.StringVar(&l.model, "model", "", "fix leaf model")
	fs.StringVar(&l.sha, "sha", "", "commit of the fix; default HEAD")
	fs.StringVar(&l.dispatch, "dispatch", "", "fill tokens, duration, model from a dispatch id")
	fs.IntVar(&l.tokensIn, "tokens-in", 0, "fix leaf input tokens")
	fs.IntVar(&l.tokensOut, "tokens-out", 0, "fix leaf output tokens")
	fs.IntVar(&l.tokensCache, "tokens-cache", 0, "fix leaf cache tokens")
	fs.IntVar(&l.attempts, "attempts", 1, "fix attempts")
	fs.Float64Var(&l.durationS, "duration-s", 0, "fix leaf duration in seconds")
	fs.Float64Var(&l.savingMSDay, "saving-ms-day", 0, "measured saving, ms per day")
}

func runLoop(o options, stdout, stderr io.Writer) int {
	l := o.loop
	var err error
	switch {
	case l.verdicts:
		err = runVerdicts(l, o.json, stdout)
	case l.queue:
		err = runQueue(l, o.json, stdout)
	case l.decide:
		return runDecide(l, stdout, stderr)
	default:
		err = recordRow(l)
	}
	if err != nil {
		fmt.Fprintln(stderr, "review bench:", err)
		return 1
	}
	return 0
}

// Item is one queue entry; Weak items (a noisy or slow benchmark) outrank fix candidates.
type Item struct {
	Kind        string  `json:"kind"`
	Pkg         string  `json:"pkg"`
	What        string  `json:"what"`
	CallsPerDay float64 `json:"calls_per_day"`
	EstSavingMS float64 `json:"est_saving_ms"`
	Source      string  `json:"source"`
}

func (i Item) score() float64 { return i.CallsPerDay * i.EstSavingMS }

type queueLine struct {
	Source   string  `json:"source"`
	Module   string  `json:"module"`
	Pkg      string  `json:"pkg"`
	BeforeMS float64 `json:"before_ms"`
	AfterMS  float64 `json:"after_ms"`
}

// Row is one scorecard line.
type Row struct {
	At          time.Time `json:"at"`
	Pkg         string    `json:"pkg"`
	What        string    `json:"what"`
	Verdict     string    `json:"verdict"`
	Agent       string    `json:"agent"`
	Model       string    `json:"model"`
	TokensIn    int       `json:"tokens_in"`
	TokensOut   int       `json:"tokens_out"`
	TokensCache int       `json:"tokens_cache,omitempty"`
	DurationS   float64   `json:"duration_s"`
	Attempts    int       `json:"attempts"`
	SavingMSDay float64   `json:"saving_ms_day"`
	Sha         string    `json:"sha,omitempty"`
	Benchmark   string    `json:"benchmark,omitempty"`
	Epoch       string    `json:"epoch,omitempty"`
	OldEpoch    string    `json:"old_epoch,omitempty"`
}

func benchRoot() string { return filepath.Join(paths.StateDir(), "bench") }

func pkgDirName(pkg string) string {
	if filepath.Clean(pkg) == "." {
		return "root"
	}
	return strings.ReplaceAll(strings.TrimPrefix(pkg, "internal/"), "/", "-")
}

const seedCandidates = `{"pkg":"cmd/claude-code","what":"hook process init: lazy sqlexec, anthropic, jsonschema, models, providers","calls_per_day":13000,"est_saving_ms":0.4,"source":"baseline"}
{"pkg":"internal/hooks/safety","what":"settings/policy cold parse: sized read, one decode","calls_per_day":12800,"est_saving_ms":0.15,"source":"baseline"}
{"pkg":"internal/hooks/safety","what":"PermissionGate foldedGlobs/anchorGlob memo per policy load","calls_per_day":3000,"est_saving_ms":0.15,"source":"baseline"}
{"pkg":"internal/providers","what":"registry JSON decoded three times: decode once, derive both orders","calls_per_day":2000,"est_saving_ms":0.05,"source":"baseline"}
{"pkg":"internal/hooks","what":"dispatch-effort not profiled: profile, then fix the top frame","calls_per_day":500,"est_saving_ms":2.5,"source":"baseline"}
`

func seedFile(file string) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	return paths.AppendLine(file, []byte(seedCandidates))
}

// readJSONL skips undecodable lines; a missing file is empty.
func readJSONL[T any](file string) ([]T, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var v T
		if json.Unmarshal(sc.Bytes(), &v) == nil {
			out = append(out, v)
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("read %s: %w", file, err)
	}
	return out, nil
}

var procSuffix = regexp.MustCompile(`-\d+$`)

// collect gathers every candidate from the queue, post-commit results and seeds.
func collect(root string) ([]Item, error) {
	cand := filepath.Join(root, "candidates.jsonl")
	if err := seedFile(cand); err != nil {
		return nil, err
	}
	items, err := readJSONL[Item](cand)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Kind = "fix"
	}
	queue, err := readJSONL[queueLine](filepath.Join(root, "queue.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, q := range queue {
		items = append(items, Item{Kind: "fix", Pkg: q.Pkg, What: q.Module + " p95 rose",
			CallsPerDay: 1, EstSavingMS: q.AfterMS - q.BeforeMS, Source: "queue:" + q.Source})
	}
	files, _ := filepath.Glob(filepath.Join(root, "post-commit", "*.json"))
	for _, f := range files {
		if strings.HasSuffix(f, ".in.json") {
			continue
		}
		var rep Report
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &rep) == nil {
			items = append(items, fromReport(rep, filepath.Base(f))...)
		}
	}
	return items, nil
}

func fromReport(rep Report, src string) []Item {
	var out []Item
	for _, p := range rep.Packages {
		for _, s := range p.Benchmarks {
			name := procSuffix.ReplaceAllString(s.Name, "")
			for _, fl := range s.Flags {
				if fl == "noisy" || fl == "slow" {
					out = append(out, Item{Kind: "weak", Pkg: p.Pkg, What: name + " is " + fl, Source: src})
				}
			}
			if d, ok := s.Deltas["sec/op"]; ok && d.Significant && strings.HasPrefix(d.Change, "+") {
				out = append(out, Item{Kind: "fix", Pkg: p.Pkg, What: name + " regressed " + d.Change, Source: src})
			}
		}
	}
	return out
}

const revertedHold = 7 * 24 * time.Hour

func closeKey(pkg, what string) string {
	head, _, _ := strings.Cut(what, ":")
	return pkg + "\x00" + strings.ToLower(strings.TrimSpace(head))
}

// closedUntil maps pkg+what head to when its row stops closing it (zero time = for good).
func closedUntil(rows []Row) map[string]time.Time {
	closed := map[string]time.Time{}
	for _, r := range rows {
		k := closeKey(r.Pkg, r.What)
		switch r.Verdict {
		case "kept":
			closed[k] = time.Time{}
		case "reverted":
			if until, ok := closed[k]; !ok || (!until.IsZero() && r.At.Add(revertedHold).After(until)) {
				closed[k] = r.At.Add(revertedHold)
			}
		}
	}
	return closed
}

func cmpItems(a, b Item) int {
	if wa, wb := a.Kind == "weak", b.Kind == "weak"; wa != wb {
		return cmp.Compare(btoi(wb), btoi(wa))
	}
	if c := cmp.Compare(b.score(), a.score()); c != 0 {
		return c
	}
	return strings.Compare(a.Pkg+a.What, b.Pkg+b.What)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rank drops items the scorecard closed, merges duplicates, and orders weak first then by score.
func rank(items []Item, rows []Row, now time.Time) []Item {
	closed := closedUntil(rows)
	best := map[string]Item{}
	for _, it := range items {
		k := it.Pkg + "\x00" + it.What
		if until, ok := closed[closeKey(it.Pkg, it.What)]; ok && (until.IsZero() || now.Before(until)) {
			continue
		}
		if cur, ok := best[k]; !ok || it.score() > cur.score() {
			best[k] = it
		}
	}
	out := slices.Collect(maps.Values(best))
	slices.SortFunc(out, cmpItems)
	return out
}

func runQueue(l loopOpts, asJSON bool, w io.Writer) error {
	root := benchRoot()
	items, err := collect(root)
	if err != nil {
		return err
	}
	rows, err := readJSONL[Row](filepath.Join(root, "scorecard.jsonl"))
	if err != nil {
		return err
	}
	ranked := rank(items, rows, time.Now())
	if l.top > 0 && len(ranked) > l.top {
		ranked = ranked[:l.top]
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(ranked)
	}
	for i, it := range ranked {
		fmt.Fprintf(w, "%d. [%s] %s: %s  (%.0f calls/day x %.2f ms = %.1f s/day; %s)\n", i+1, it.Kind, it.Pkg,
			it.What, it.CallsPerDay, it.EstSavingMS, it.score()/1000, it.Source)
	}
	if len(ranked) == 0 {
		fmt.Fprintln(w, "queue empty")
	}
	return nil
}

// Decision is the keep/revert verdict and the benchstat rows that decided it.
type Decision struct {
	Keep         bool
	NoComparison bool // no benchmark is in both runs, so neither KEEP nor REVERT
	Rule         string
	Rows         []string
}

const exitNoComparison = 3

func isQuiet(s Score) bool {
	return !slices.Contains(s.Flags, "noisy") && !slices.Contains(s.Flags, "load")
}

func quietBoth(before, after []Score) map[string]bool {
	quiet := map[string]bool{}
	for _, s := range before {
		quiet[strings.TrimPrefix(s.Name, "Benchmark")] = isQuiet(s)
	}
	for _, s := range after {
		n := strings.TrimPrefix(s.Name, "Benchmark")
		quiet[n] = quiet[n] && isQuiet(s)
	}
	return quiet
}

// minRegressionPct is the smallest significant regression, in percent, that counts toward REVERT.
const minRegressionPct = 2.0

// changePct is the size of a benchstat change such as "+1.25%" or "-15.76%", in percent.
func changePct(change string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimLeft(change, "+-"), "%"), 64)
	return v
}

type benchRow struct {
	unit, text string
	pct        float64
}

// sortRows splits significant deltas into wins (sec/op only on quiet runs) and regressions.
func sortRows(deltas map[string]map[string]Delta, quiet map[string]bool) (win, loss []benchRow) {
	for name, units := range deltas {
		for _, u := range []string{"sec/op", "B/op", "allocs/op"} {
			d := units[u]
			r := benchRow{u, fmt.Sprintf("%s %s %s -> %s %s (%s)", name, u, d.Old, d.New, d.Change, d.P), changePct(d.Change)}
			switch {
			case !d.Significant:
			case strings.HasPrefix(d.Change, "+"):
				loss = append(loss, r)
			case u != "sec/op" || quiet[name]:
				win = append(win, r)
			}
		}
	}
	return win, loss
}

// bestSpeedup is the largest sec/op win, or 0 when a sec/op row regresses by minRegressionPct or more.
func bestSpeedup(win, loss []benchRow) float64 {
	for _, r := range loss {
		if r.unit == "sec/op" && r.pct >= minRegressionPct {
			return 0
		}
	}
	best := 0.0
	for _, r := range win {
		if r.unit == "sec/op" {
			best = max(best, r.pct)
		}
	}
	return best
}

// decide keeps on a significant win with no regression that counts: under 2% is minor, a speedup outweighs less memory.
func decide(deltas map[string]map[string]Delta, before, after []Score) Decision {
	win, loss := sortRows(deltas, quietBoth(before, after))
	speed := bestSpeedup(win, loss)
	var counted, outweighed, minor, wins []string
	for _, r := range loss {
		switch {
		case r.pct < minRegressionPct:
			minor = append(minor, "minor: "+r.text)
		case r.unit != "sec/op" && r.pct <= speed:
			outweighed = append(outweighed, "outweighed: "+r.text)
		default:
			counted = append(counted, "regressed: "+r.text)
		}
	}
	for _, r := range win {
		wins = append(wins, "improved: "+r.text)
	}
	for _, s := range [][]string{counted, outweighed, minor, wins} {
		slices.Sort(s)
	}
	d := Decision{Keep: len(wins) > 0 && len(counted) == 0, Rule: decideRule(len(wins), counted, outweighed, minor, speed)}
	d.Rows = slices.Concat(counted, outweighed, minor, wins)
	return d
}

// decideRule names the rule that decided the verdict.
func decideRule(wins int, counted, outweighed, minor []string, speed float64) string {
	switch {
	case len(counted) > 0:
		return fmt.Sprintf("a significant regression of %.0f%% or more reverts", minRegressionPct)
	case wins == 0:
		return "no significant improvement"
	case len(outweighed) > 0:
		return fmt.Sprintf("time-weighted: the -%.2f%% sec/op win outweighs smaller B/op and allocs/op regressions", speed)
	case len(minor) > 0:
		return fmt.Sprintf("regressions under %.0f%% are minor and do not count", minRegressionPct)
	}
	return "significant improvement, no regression"
}

var headerNum = regexp.MustCompile(`(?m)^# (cpus|load-before|load-after): ([0-9.]+)`)

// loadNoisy reports whether a stored run's header shows load above half the CPUs.
func loadNoisy(text string) bool {
	var cpus, load float64
	for _, m := range headerNum.FindAllStringSubmatch(text, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		if m[1] == "cpus" {
			cpus = v
		} else {
			load = max(load, v)
		}
	}
	return cpus > 0 && load > cpus/2
}

// benchNames are a run text's benchmark names as benchstat keys them (no "Benchmark" prefix).
func benchNames(text string) map[string]bool {
	names, _ := parseSamples(text)
	out := map[string]bool{}
	for _, n := range names {
		out[strings.TrimPrefix(n, "Benchmark")] = true
	}
	return out
}

// decideFiles decides on benchmarks both runs hold; one-sided ones are listed, and none in common is NoComparison.
func decideFiles(before, after string) (Decision, error) {
	bt, err := os.ReadFile(before)
	if err != nil {
		return Decision{}, err
	}
	at, err := os.ReadFile(after)
	if err != nil {
		return Decision{}, err
	}
	if !sameEpochs(string(bt), string(at)) {
		return Decision{NoComparison: true, Rule: "benchmark epochs differ; rebaseline before comparing"}, nil
	}
	miss := missingBench(string(bt), string(at))
	bn, an := benchNames(string(bt)), benchNames(string(at))
	common := false
	for n := range bn {
		common = common || an[n]
	}
	if !common {
		return Decision{NoComparison: true, Rows: miss}, nil
	}
	deltas, err := compare(before, after)
	if err != nil {
		return Decision{}, err
	}
	maps.DeleteFunc(deltas, func(n string, _ map[string]Delta) bool { return !bn[n] || !an[n] })
	d := decide(deltas, scoreAll(string(bt), loadNoisy(string(bt))), scoreAll(string(at), loadNoisy(string(at))))
	d.Rows = append(d.Rows, miss...)
	return d, nil
}

func runDecide(l loopOpts, stdout, stderr io.Writer) int {
	if l.before == "" || l.after == "" {
		fmt.Fprintln(stderr, "review bench: --decide needs --before and --after")
		return 2
	}
	d, err := decideFiles(l.before, l.after)
	if err != nil {
		fmt.Fprintln(stderr, "review bench:", err)
		return 1
	}
	verdict, code := "REVERT", 1
	switch {
	case d.NoComparison:
		verdict, code = "NO COMPARISON: no comparable benchmark", exitNoComparison
	case d.Keep:
		verdict, code = "KEEP", 0
	}
	fmt.Fprintln(stdout, verdict)
	if d.Rule != "" {
		fmt.Fprintln(stdout, "  rule: "+d.Rule)
	}
	for _, r := range d.Rows {
		fmt.Fprintln(stdout, "  "+r)
	}
	return code
}

func advanceRolling(pkg, after string) error {
	b, err := os.ReadFile(after)
	if err != nil {
		return err
	}
	dir := filepath.Join(benchRoot(), pkgDirName(pkg))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeRolling(dir, b)
}

// writeRolling replaces dir/rolling.txt atomically under the package lock.
func writeRolling(dir string, body []byte) error {
	file := filepath.Join(dir, "rolling.txt")
	return paths.WithFileLockWithin(file, rollingLock, func() error {
		return paths.AtomicWriteNoSync(file, body, 0o644)
	})
}

// dispatchRecord is the slice of an `agents ask` record a scorecard row needs; internal/agent imports this package.
type dispatchRecord struct {
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	DurationMs int64  `json:"duration_ms"`
	Usage      *struct {
		In         int `json:"input_tokens"`
		CacheWrite int `json:"cache_creation_tokens"`
		CacheRead  int `json:"cache_read_tokens"`
		Out        int `json:"output_tokens"`
	} `json:"usage"`
}

// fromDispatch fills the row fields left unset from the dispatch record with that exact id.
func fromDispatch(l loopOpts) (loopOpts, error) {
	if l.dispatch == "" {
		return l, nil
	}
	if l.dispatch != filepath.Base(l.dispatch) || strings.Contains(l.dispatch, "..") {
		return l, fmt.Errorf("--dispatch %q: not a dispatch id", l.dispatch)
	}
	b, err := os.ReadFile(filepath.Join(paths.DispatchDir(), l.dispatch+".json"))
	if err != nil {
		return l, fmt.Errorf("--dispatch: %w", err)
	}
	var r dispatchRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return l, fmt.Errorf("--dispatch %s: %w", l.dispatch, err)
	}
	l.agent, l.model = cmp.Or(l.agent, r.Agent), cmp.Or(l.model, r.Model)
	l.durationS = cmp.Or(l.durationS, float64(r.DurationMs)/1e3)
	if u := r.Usage; u != nil {
		l.tokensIn, l.tokensOut = cmp.Or(l.tokensIn, u.In), cmp.Or(l.tokensOut, u.Out)
		l.tokensCache = cmp.Or(l.tokensCache, u.CacheWrite+u.CacheRead)
	}
	return l, nil
}

func recordRow(l loopOpts) error {
	kept := l.verdict == "kept"
	if err := validateEvidence(l); err != nil {
		return err
	}
	if l.sha == "" {
		l.sha, _ = source.GitInherit(".", "rev-parse", "--short", "HEAD")
	}
	l, err := fromDispatch(l)
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	data, err := json.Marshal(Row{At: at, Pkg: l.pkg, What: l.what, Verdict: l.verdict, Agent: l.agent,
		Model: l.model, TokensIn: l.tokensIn, TokensOut: l.tokensOut, TokensCache: l.tokensCache,
		DurationS: l.durationS, Attempts: l.attempts, SavingMSDay: l.savingMSDay, Sha: l.sha,
		Benchmark: l.benchmark, Epoch: l.epoch, OldEpoch: l.oldEpoch})
	if err != nil {
		return err
	}
	if kept && l.after != "" { // a fix with no benchmark (process init) keeps its baseline as is
		if err := advanceRolling(l.pkg, l.after); err != nil {
			return err
		}
	}
	if err := paths.AppendLine(filepath.Join(benchRoot(), "scorecard.jsonl"), data); err != nil {
		return err
	}
	return recordEvidence(l, at)
}
