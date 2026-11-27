package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	"unicode"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/review/bench"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/settings"
)

const (
	dreamRtkTimeout = 60 * time.Second
	dreamRtkTop     = 10
)

var (
	rtkWordRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)
	rtkFlagRe = regexp.MustCompile(`^-(?:[A-Za-z]|-[a-z][a-z-]{1,20})$`) // one bare flag; "-ePAT" or "--k=v" carry values
)

type rtkMiss struct {
	Command string `json:"command"`
	Count   int    `json:"count"`
}

// rtkMissWord keeps the command word and its first flag only, never paths or arguments.
func rtkMissWord(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 || !rtkWordRe.MatchString(f[0]) {
		return ""
	}
	if len(f) > 1 && rtkFlagRe.MatchString(f[1]) {
		return f[0] + " " + f[1]
	}
	return f[0]
}

// readDreamRtk lists the top commands run without rtk over the last 7 days.
var readDreamRtk = func() ([]rtkMiss, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dreamRtkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rtk", "discover", "--all", "--since", "7", "--format", "json")
	cmd.Env = append(os.Environ(), "RTK_SUPPRESS_HOOK_WARNING=1", "RTK_TELEMETRY_DISABLED=1")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rtk discover skipped: %w", err)
	}
	var doc struct {
		Supported []rtkMiss `json:"supported"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("rtk discover skipped: bad json: %w", err)
	}
	var out []rtkMiss
	for _, m := range doc.Supported {
		if w := rtkMissWord(m.Command); w != "" && m.Count > 0 && len(out) < dreamRtkTop {
			out = append(out, rtkMiss{Command: w, Count: m.Count})
		}
	}
	return out, nil
}

func writeDreamRtk(out io.Writer, misses []rtkMiss, err error) {
	fmt.Fprintf(out, "memory dream: rtk misses: %d command(s)\n", len(misses))
	for _, m := range misses {
		fmt.Fprintf(out, "memory dream: rtk miss %s x%d\n", m.Command, m.Count)
	}
	dreamStepError(out, "rtk discover", err)
}

// rtkPromptBlock asks the agent for one rtk candidate record from the misses; empty when there are none.
func rtkPromptBlock(misses []rtkMiss) string {
	if len(misses) == 0 {
		return ""
	}
	b, _ := json.Marshal(misses)
	return assets.MemoryRTKMisses(string(b))
}

const (
	dreamP95Window       = 7 * 24 * time.Hour
	dreamP95MinRuns      = 200
	dreamP95Growth       = 1.2
	dreamBenchQueueAge   = 14 * 24 * time.Hour
	dreamBenchQueueLimit = 200
)

type dreamBenchEntry struct {
	At       time.Time              `json:"at"`
	Source   string                 `json:"source"`
	Module   string                 `json:"module"`
	Pkg      string                 `json:"pkg"`
	BeforeMS float64                `json:"before_ms"`
	AfterMS  float64                `json:"after_ms"`
	Deltas   map[string]bench.Delta `json:"deltas,omitempty"`
}

type dreamHookRow struct {
	T      time.Time `json:"t"`
	Module string    `json:"module"`
	MS     *float64  `json:"ms"`
	Sid    string    `json:"sid"`
	N      int       `json:"n"`
}

type dreamHookWindows struct{ before, after []float64 }

func readDreamP95(dir string, now time.Time) ([]dreamBenchEntry, error) {
	windows := map[string]*dreamHookWindows{}
	start := now.Add(-2 * dreamP95Window)
	var errs []error
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, now.Location())
	for ; !day.After(now); day = day.AddDate(0, 0, 1) {
		file := filepath.Join(dir, "telemetry-"+day.Format("20060102")+".jsonl")
		f, err := os.Open(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = scanDreamP95(f, now, windows)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(file), err))
		}
		if err := f.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	var flags []dreamBenchEntry
	modules := make([]string, 0, len(windows))
	for module := range windows {
		modules = append(modules, module)
	}
	slices.Sort(modules)
	for _, module := range modules {
		w := windows[module]
		if min(len(w.before), len(w.after)) < dreamP95MinRuns {
			continue
		}
		before, after := dreamP95(w.before), dreamP95(w.after)
		if after > before*dreamP95Growth {
			flags = append(flags, dreamBenchEntry{At: now, Source: "dream-p95", Module: module,
				Pkg: dreamHookPackage(module), BeforeMS: before, AfterMS: after})
		}
	}
	return flags, errors.Join(errs...)
}

func scanDreamP95(r io.Reader, now time.Time, windows map[string]*dreamHookWindows) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 1<<20)
	invalid, folded := 0, 0
	for scanner.Scan() {
		var row dreamHookRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			invalid++
			continue
		}
		if row.outsideDreamWindow(now) {
			continue
		}
		if row.N > 1 {
			folded++
			continue
		} // Folded durations are maxima, not individual observations.
		if !row.validDreamDuration() {
			invalid++
			continue
		}
		w := windows[row.Module]
		if w == nil {
			w = &dreamHookWindows{}
			windows[row.Module] = w
		}
		if row.T.Before(now.Add(-dreamP95Window)) {
			w.before = append(w.before, *row.MS)
		} else {
			w.after = append(w.after, *row.MS)
		}
	}
	var err error
	if invalid+folded > 0 {
		err = fmt.Errorf("ignored %d invalid and %d folded rows", invalid, folded)
	}
	return errors.Join(err, scanner.Err())
}

func (row dreamHookRow) outsideDreamWindow(now time.Time) bool {
	return row.Sid == "livechec" || row.T.Before(now.Add(-2*dreamP95Window)) || !row.T.Before(now)
}

func (row dreamHookRow) validDreamDuration() bool {
	return row.Module != "" && row.MS != nil && *row.MS >= 0 && !math.IsInf(*row.MS, 0)
}

func dreamP95(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[len(sorted)-1-len(sorted)/20]
}

func dreamHookPackage(module string) string {
	groups := map[string]string{
		"safety": "safety", "safety-edit": "safety", "permission": "safety", "rtk": "safety",
		"docs": "docs", "paths": "docs", "memory": "memhook", "format": "quality", "review": "quality",
		"worktree": "wthook",
	}
	for _, group := range settings.Registry {
		if !slices.ContainsFunc(group.Events, func(s settings.EventSpec) bool { return s.Name == module }) {
			continue
		}
		if module == "docs-plan-reminder" || module == "worktree-busy" {
			return "internal/hooks"
		}
		if sub, ok := groups[group.Name]; ok {
			return "internal/hooks/" + sub
		}
		return "internal/hooks"
	}
	return module
}

func dreamP95Line(flag dreamBenchEntry) string {
	return fmt.Sprintf("hook p95 %s: %.3f -> %.3f ms (>20%%; >=%d runs/window)", flag.Module, flag.BeforeMS, flag.AfterMS, dreamP95MinRuns)
}

func writeDreamP95(out io.Writer, flags []dreamBenchEntry, err error) {
	fmt.Fprintf(out, "memory dream: hook p95: %d regression(s)\n", len(flags))
	for _, flag := range flags {
		fmt.Fprintln(out, "memory dream:", dreamP95Line(flag))
	}
	dreamStepError(out, "hook p95", err)
}

func dreamStepError(out io.Writer, step string, err error) {
	if err != nil {
		fmt.Fprintf(out, "memory dream: %s: %s\n", step, strings.Join(strings.Fields(err.Error()), " "))
	}
}

// ReportFailure is set by internal/worktree (memory cannot import it); nil is a no-op.
var ReportFailure func(kind, subject, detail string) error

func dreamPerformance(out io.Writer) []string {
	flags, err := readDreamP95(filepath.Join(paths.StateDir(), "hooks"), time.Now())
	writeDreamP95(out, flags, err)
	var notice []string
	for _, flag := range flags {
		notice = append(notice, dreamP95Line(flag))
		dreamStepError(out, "hook p95 queue", queueDreamBench(flag))
		if ReportFailure != nil {
			dreamStepError(out, "hook p95 failure", ReportFailure("hook_p95", flag.Module, dreamP95Line(flag)))
		}
	}
	dreamStepError(out, "rotation", dreamBenchRotation(out))
	return notice
}

func queueDreamBench(entry dreamBenchEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file := filepath.Join(paths.StateDir(), "bench", "queue.jsonl")
	return paths.WithFileLockWithin(file, 5*time.Second, func() error {
		kept, err := compactDreamBenchQueue(file, entry, time.Now())
		if err != nil {
			return err
		}
		if err := paths.AtomicWrite(file, kept, 0o600); err != nil {
			return err
		}
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(data, '\n'))
		return errors.Join(err, f.Close())
	})
}

func compactDreamBenchQueue(file string, incoming dreamBenchEntry, now time.Time) ([]byte, error) {
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	type row struct {
		entry dreamBenchEntry
		line  string
	}
	latest := map[string]row{}
	cutoff := now.Add(-dreamBenchQueueAge)
	incomingKey := dreamBenchQueueKey(incoming)
	for line := range strings.SplitSeq(string(data), "\n") {
		var entry dreamBenchEntry
		if json.Unmarshal([]byte(line), &entry) != nil || entry.At.Before(cutoff) {
			continue
		}
		key := dreamBenchQueueKey(entry)
		if key == incomingKey {
			continue
		}
		if entry.At.Before(latest[key].entry.At) {
			continue
		}
		latest[key] = row{entry: entry, line: line}
	}
	rows := make([]row, 0, len(latest))
	for _, r := range latest {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b row) int { return a.entry.At.Compare(b.entry.At) })
	rows = rows[max(0, len(rows)-dreamBenchQueueLimit+1):]
	var kept []byte
	for _, row := range rows {
		kept = append(append(kept, row.line...), '\n')
	}
	return kept, nil
}

func dreamBenchQueueKey(entry dreamBenchEntry) string {
	name := entry.Module
	if name == "" {
		name = entry.Pkg
	}
	return entry.Source + "\x00" + name
}

const dreamRotationSize = 2

type dreamBenchState struct {
	RankedAt time.Time `json:"ranked_at"`
	Ranking  []string  `json:"ranking"`
	Last     string    `json:"last"`
}

type dreamGoPackage struct {
	Dir          string
	ImportPath   string
	Imports      []string
	TestGoFiles  []string
	XTestGoFiles []string
}

func dreamBenchRotation(out io.Writer) error {
	root := paths.ConfigDir()
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("skipped: harness source: %w", err)
	}
	dir := filepath.Join(paths.StateDir(), "bench")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, "rotation.lock"), 0, func() error {
		return rotateDreamBench(root, filepath.Join(dir, "rotation.json"), out)
	})
	if !ran && err == nil {
		fmt.Fprintln(out, "memory dream: rotation: skipped (already running)")
	}
	return err
}

func rotateDreamBench(root, file string, out io.Writer) error {
	state, err := loadDreamBenchRanking(root, file)
	if err != nil {
		return err
	}
	selected := nextDreamBenchPackages(state.Ranking, state.Last)
	if len(selected) == 0 {
		fmt.Fprintln(out, "memory dream: rotation: skipped (no benchmark packages)")
	}
	var errs []error
	for _, pkg := range selected {
		if err := dreamBenchIdle(); err != nil {
			dreamStepError(out, "rotation: skipped "+pkg, err)
			break
		}
		err := runDreamBench(root, pkg, out)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pkg, err))
		}
		state.Last = pkg
		if err := saveDreamBenchState(file, state); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

func loadDreamBenchRanking(root, file string) (dreamBenchState, error) {
	state, err := readDreamBenchState(file)
	if err != nil {
		return state, err
	}
	if age := time.Since(state.RankedAt); age >= 0 && age < 24*time.Hour {
		return state, nil
	}
	state.Ranking, err = rankDreamBenchPackages(root)
	if err != nil {
		return state, err
	}
	state.RankedAt = time.Now()
	return state, saveDreamBenchState(file, state)
}

func readDreamBenchState(file string) (dreamBenchState, error) {
	var state dreamBenchState
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("rotation state: %w", err)
	}
	for _, pkg := range state.Ranking {
		if !filepath.IsLocal(pkg) || strings.Contains(pkg, `\`) {
			return state, fmt.Errorf("rotation package is not local: %q", pkg)
		}
	}
	return state, nil
}

func saveDreamBenchState(file string, state dreamBenchState) error {
	return paths.WriteJSON(file, state)
}

func nextDreamBenchPackages(ranking []string, last string) []string {
	if len(ranking) == 0 {
		return nil
	}
	start := (slices.Index(ranking, last) + 1) % len(ranking)
	selected := make([]string, 0, min(dreamRotationSize, len(ranking)))
	for i := range cap(selected) {
		selected = append(selected, ranking[(start+i)%len(ranking)])
	}
	return selected
}

func rankDreamBenchPackages(root string) ([]string, error) {
	pkgs, err := listDreamBenchPackages(root)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, pkg := range pkgs {
		for _, imported := range pkg.Imports {
			counts[imported]++
		}
	}
	slices.SortFunc(pkgs, func(a, b dreamGoPackage) int {
		if n := counts[b.ImportPath] - counts[a.ImportPath]; n != 0 {
			return n
		}
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
	var ranking []string
	for _, pkg := range pkgs {
		has, err := dreamPackageHasBench(pkg)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		rel, err := filepath.Rel(root, pkg.Dir)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("go list directory outside harness: %s", pkg.Dir)
		}
		ranking = append(ranking, filepath.ToSlash(rel))
	}
	return ranking, nil
}

func listDreamBenchPackages(root string) ([]dreamGoPackage, error) {
	data, err := dreamBenchCommand(root, time.Minute, "go", "list", "-json", "./...")
	if err != nil {
		return nil, fmt.Errorf("importer ranking: %w", err)
	}
	var pkgs []dreamGoPackage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg dreamGoPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list JSON: %w", err)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func dreamPackageHasBench(pkg dreamGoPackage) (bool, error) {
	files := append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...)
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && dreamBenchmarkName(fn.Name.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}

func dreamBenchmarkName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Benchmark")
	if !ok {
		return false
	}
	for _, r := range rest {
		return !unicode.IsLower(r)
	}
	return true
}

func dreamBenchIdle() error {
	var data []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		data, err = os.ReadFile("/proc/loadavg")
	case "darwin":
		data, err = dreamBenchCommand("", 5*time.Second, "sysctl", "-n", "vm.loadavg")
	default:
		return fmt.Errorf("load average unavailable on %s", runtime.GOOS)
	}
	if err != nil {
		return fmt.Errorf("load average unavailable: %w", err)
	}
	fields := strings.Fields(strings.Trim(string(data), "{} \n\t"))
	if len(fields) == 0 {
		return errors.New("load average is empty")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || !(load >= 0 && load < float64(runtime.NumCPU())/2) {
		return fmt.Errorf("load %s is not below %.1f", fields[0], float64(runtime.NumCPU())/2)
	}
	return nil
}

func dreamBenchCommand(root string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.WaitDelay = root, time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

func runDreamBench(root, pkg string, out io.Writer) error {
	data, err := dreamBenchCommand(root, 35*time.Minute, "claude-code", "review", "bench", "--path", filepath.Join(root, pkg), "--count", "5", "--format", "json")
	if err != nil {
		return err
	}
	var report bench.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("review bench JSON: %w", err)
	}
	queued := 0
	for _, result := range report.Packages {
		for _, score := range result.Benchmarks {
			if !dreamBenchMoved(score) {
				continue
			}
			entry := dreamBenchEntry{At: time.Now(), Source: "dream-rotation", Module: score.Name, Pkg: result.Pkg, Deltas: score.Deltas}
			if d, ok := score.Deltas["sec/op"]; ok {
				before, e1 := strconv.ParseFloat(d.Old, 64)
				after, e2 := strconv.ParseFloat(d.New, 64)
				if err := errors.Join(e1, e2); err != nil {
					return fmt.Errorf("benchmark duration: %w", err)
				}
				const msPerSecond = float64(time.Second / time.Millisecond)
				entry.BeforeMS, entry.AfterMS = before*msPerSecond, after*msPerSecond
			}
			if err := queueDreamBench(entry); err != nil {
				return err
			}
			queued++
		}
	}
	fmt.Fprintf(out, "memory dream: rotation: %s (%d significant benchmark(s) queued)\n", pkg, queued)
	return nil
}

func dreamBenchMoved(score bench.Score) bool {
	for _, delta := range score.Deltas {
		if delta.Significant {
			return true
		}
	}
	return false
}
