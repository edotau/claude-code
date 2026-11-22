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
	queue, decide, record  bool
	top                    int
	before, after          string
	pkg, what, verdict     string
	agent, model, sha      string
	tokensIn, tokensOut    int
	tokensCache, attempts  int
	durationS, savingMSDay float64
}

func loopFlags(fs *flag.FlagSet, l *loopOpts) {
	fs.BoolVar(&l.queue, "queue", false, "rank the next fix candidates from the queue, results and seeds")
	fs.BoolVar(&l.decide, "decide", false, "KEEP/REVERT from --before and --after (exit 0 keep, 1 revert)")
	fs.BoolVar(&l.record, "record", false, "append a scorecard row; on kept, --after (if given) becomes rolling.txt")
	fs.IntVar(&l.top, "top", 5, "queue items to print")
	fs.StringVar(&l.before, "before", "", "benchmark file before the fix")
	fs.StringVar(&l.after, "after", "", "benchmark file after the fix")
	fs.StringVar(&l.pkg, "pkg", "", "package dir, e.g. internal/hooks/safety")
	fs.StringVar(&l.what, "what", "", "the candidate the fix addressed")
	fs.StringVar(&l.verdict, "verdict", "", "kept|reverted")
	fs.StringVar(&l.agent, "agent", "", "fix leaf agent")
	fs.StringVar(&l.model, "model", "", "fix leaf model")
	fs.StringVar(&l.sha, "sha", "", "commit of the fix; default HEAD")
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
}

func benchRoot() string { return filepath.Join(paths.StateDir(), "bench") }

func pkgDirName(pkg string) string {
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

func readJSONL[T any](file string) []T {
	f, err := os.Open(file)
	if err != nil {
		return nil
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
	return out
}

var procSuffix = regexp.MustCompile(`-\d+$`)

// collect gathers every candidate from the queue, post-commit results and seeds.
func collect(root string) ([]Item, error) {
	cand := filepath.Join(root, "candidates.jsonl")
	if err := seedFile(cand); err != nil {
		return nil, err
	}
	items := readJSONL[Item](cand)
	for i := range items {
		items[i].Kind = "fix"
	}
	for _, q := range readJSONL[queueLine](filepath.Join(root, "queue.jsonl")) {
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

func settledAt(rows []Row) map[string]time.Time {
	settled := map[string]time.Time{}
	for _, r := range rows {
		k := r.Pkg + "\x00" + r.What
		if (r.Verdict == "kept" || r.Verdict == "reverted") && r.At.After(settled[k]) {
			settled[k] = r.At
		}
	}
	return settled
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

// rank drops items already settled in the scorecard, merges duplicates, and orders weak first then by score.
func rank(items []Item, rows []Row, changedSince func(pkg string, at time.Time) bool) []Item {
	settled := settledAt(rows)
	best := map[string]Item{}
	for _, it := range items {
		k := it.Pkg + "\x00" + it.What
		if at, ok := settled[k]; ok && !changedSince(it.Pkg, at) {
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

func gitChangedSince(top string) func(string, time.Time) bool {
	return func(pkg string, at time.Time) bool {
		out, err := source.GitInherit(top, "log", "-1", "--format=%cI", "--", pkg)
		if err != nil || out == "" {
			return false
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
		return err == nil && t.After(at)
	}
}

func runQueue(l loopOpts, asJSON bool, w io.Writer) error {
	top, err := repoTop()
	if err != nil {
		return err
	}
	root := benchRoot()
	items, err := collect(root)
	if err != nil {
		return err
	}
	ranked := rank(items, readJSONL[Row](filepath.Join(root, "scorecard.jsonl")), gitChangedSince(top))
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
	Keep bool
	Rows []string
}

func isQuiet(s Score) bool {
	return !slices.Contains(s.Flags, "noisy") && !slices.Contains(s.Flags, "load")
}

// decide keeps on a significant ns/op win (quiet run) or a significant allocs/op or B/op drop, with no regression.
func decide(deltas map[string]map[string]Delta, after []Score) Decision {
	quiet := map[string]bool{}
	for _, s := range after {
		quiet[strings.TrimPrefix(s.Name, "Benchmark")] = isQuiet(s)
	}
	var win, loss []string
	for name, units := range deltas {
		for _, u := range []string{"sec/op", "B/op", "allocs/op"} {
			d := units[u]
			row := fmt.Sprintf("%s %s %s -> %s %s (%s)", name, u, d.Old, d.New, d.Change, d.P)
			switch {
			case !d.Significant:
			case strings.HasPrefix(d.Change, "+"):
				loss = append(loss, "regressed: "+row)
			case u != "sec/op" || quiet[name]:
				win = append(win, "improved: "+row)
			}
		}
	}
	slices.Sort(win)
	slices.Sort(loss)
	return Decision{Keep: len(win) > 0 && len(loss) == 0, Rows: append(loss, win...)}
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

func runDecide(l loopOpts, stdout, stderr io.Writer) int {
	if l.before == "" || l.after == "" {
		fmt.Fprintln(stderr, "review bench: --decide needs --before and --after")
		return 2
	}
	text, err := os.ReadFile(l.after)
	if err != nil {
		fmt.Fprintln(stderr, "review bench:", err)
		return 1
	}
	deltas, err := compare(l.before, l.after)
	if err != nil {
		fmt.Fprintln(stderr, "review bench:", err)
		return 1
	}
	d := decide(deltas, scoreAll(string(text), loadNoisy(string(text))))
	verdict := "REVERT"
	if d.Keep {
		verdict = "KEEP"
	}
	fmt.Fprintln(stdout, verdict)
	for _, r := range d.Rows {
		fmt.Fprintln(stdout, "  "+r)
	}
	if d.Keep {
		return 0
	}
	return 1
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
	return os.WriteFile(filepath.Join(dir, "rolling.txt"), b, 0o644)
}

func recordRow(l loopOpts) error {
	kept := l.verdict == "kept"
	if l.pkg == "" || l.what == "" || !kept && l.verdict != "reverted" {
		return errors.New("--record needs --pkg, --what and --verdict kept|reverted")
	}
	if l.sha == "" {
		l.sha, _ = source.GitInherit(".", "rev-parse", "--short", "HEAD")
	}
	data, _ := json.Marshal(Row{At: time.Now().UTC(), Pkg: l.pkg, What: l.what, Verdict: l.verdict, Agent: l.agent,
		Model: l.model, TokensIn: l.tokensIn, TokensOut: l.tokensOut, TokensCache: l.tokensCache,
		DurationS: l.durationS, Attempts: l.attempts, SavingMSDay: l.savingMSDay, Sha: l.sha})
	if kept && l.after != "" { // a fix with no benchmark (process init) keeps its baseline as is
		if err := advanceRolling(l.pkg, l.after); err != nil {
			return err
		}
	}
	return paths.AppendLine(filepath.Join(benchRoot(), "scorecard.jsonl"), data)
}
