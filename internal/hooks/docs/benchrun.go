package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	benchCount     = "8" // benchstat needs at least 6 samples per side for a confidence interval
	benchField     = "harness_bench"
	benchWorkerArg = "--internal-bench-worker"
	benchPercent   = 100
	benchQuietCV   = 5
	benchMaxLines  = 4
)

// A private detached entry bypasses the hook dispatcher's five-second handler deadline.
func init() {
	if len(os.Args) == 3 && os.Args[1] == benchWorkerArg {
		base := os.Args[2]
		state := filepath.Join(paths.StateDir(), "bench", "post-commit")
		if filepath.Dir(base) == state && plans.ValidSHA(filepath.Base(base)) {
			benchWorker(base)
		}
		os.Exit(0)
	}
}

// benchSpawn starts the detached bench run; a seam so tests spawn nothing.
var benchSpawn = func(args []string, dir string) int { return proc.SpawnDetachedPID("sh", args, dir, nil) }

var benchPollWait = time.Second
var benchMaxBusyPolls = 30

// The worker owns lifecycle writes; fd 3 keeps its measurement lock alive if the worker dies mid-command.
const benchScript = `b=$1; r=$2; top=$3; sha=$4; shift 4
w=$(mktemp -d "$r.wt.XXXXXX") || exit 1
trap 'git -C "$top" worktree remove --force "$w" >/dev/null 2>&1; rm -rf "$w"' EXIT
git -C "$top" worktree add -q --detach "$w" "$sha" >/dev/null 2>&1 && cd "$w" ||
  exit 1
nice -n 19 "$b" review bench "$@" --count ` + benchCount + ` --format json >"$r" 2>"$r.err"
exit $?`

type benchJob struct {
	Top, SHA, SessionID string
	Files               []string
}

type benchMarker struct {
	Status    string    `json:"status"`
	UnderLoad bool      `json:"under_load,omitempty"`
	PID       int       `json:"pid"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Exit      *int      `json:"exit"`
	Notice    string    `json:"notice,omitempty"`
}

var benchRun = func(args []string, top string, lock *os.File) int {
	cmd := exec.Command("sh", args...)
	cmd.Dir, cmd.ExtraFiles = top, []*os.File{lock}
	if err := cmd.Run(); err != nil {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		return -1
	}
	return 0
}

var benchFuncRe = regexp.MustCompile(`(?m)^func Benchmark`)

// benchOnCommit spawns one detached `review bench` for a main-session commit that changed a benched package.
func benchOnCommit(ctx context.Context, in *core.Input, dir, sha string) {
	if runtime.GOOS == "windows" || in.AgentID != "" || !plans.ValidSHA(sha) {
		return
	}
	top := toplevel(dir)
	if top == "" {
		return
	}
	sha = primaryTrunkHead(ctx, top, sha)
	if sha == "" {
		return
	}
	if files := benchedFiles(ctx, top, sha); len(files) > 0 {
		benchStart(ctx, in, top, sha, files)
	}
}

// Admission and lock waits belong to the detached worker, never the tool's hook process.
func benchStart(_ context.Context, in *core.Input, top, sha string, files []string) {
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	if os.MkdirAll(state, 0o700) != nil {
		return
	}
	base := filepath.Join(state, sha)
	f, err := os.OpenFile(base+".job.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	err = json.NewEncoder(f).Encode(benchJob{top, sha, in.SessionID, files})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return
	}
	m := benchMarker{Status: "queued", Start: time.Now().UTC()}
	if paths.WriteJSON(base+".marker", m) != nil {
		return
	}
	bin, err := paths.SelfBinary()
	if err != nil {
		_ = benchComplete(base, m, -1)
		return
	}
	args := []string{"-c", `exec "$1" "$2" "$3"`, "sh", bin, benchWorkerArg, base}
	if benchSpawn(args, top) == 0 {
		_ = benchComplete(base, m, -1)
	}
}

func benchComplete(base string, m benchMarker, code int) error {
	m.Status, m.End, m.Exit = "failed", time.Now().UTC(), &code
	if code == 0 {
		m.Status = "done"
	}
	return paths.WriteJSON(base+".marker", m)
}

func benchReconcile(state string) {
	markers, _ := filepath.Glob(filepath.Join(state, "*.marker"))
	for _, file := range markers {
		raw, err := os.ReadFile(file)
		var m benchMarker
		if err != nil {
			continue
		}
		if json.Unmarshal(raw, &m) != nil {
			if strings.TrimSpace(string(raw)) != "running" {
				continue
			}
			m.Status = "running"
		}
		if m.abandoned() {
			_ = benchComplete(strings.TrimSuffix(file, ".marker"), m, -1)
		}
	}
}

func (m benchMarker) abandoned() bool {
	if m.Status != "running" && m.Status != "queued" {
		return false
	}
	if m.PID > 0 {
		return !proc.Alive(m.PID)
	}
	return time.Since(m.Start) > time.Minute
}

func benchWorker(base string) {
	raw, err := os.ReadFile(base + ".job.json")
	var job benchJob
	if err != nil || json.Unmarshal(raw, &job) != nil || !plans.ValidSHA(job.SHA) {
		return
	}
	m := benchMarker{Status: "queued", PID: os.Getpid(), Start: time.Now().UTC()}
	if paths.WriteJSON(base+".marker", m) != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(paths.StateDir(), "bench", "measurement.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = benchComplete(base, m, -1)
		return
	}
	defer lock.Close()
	for !paths.TryLock(lock) {
		time.Sleep(benchPollWait)
	}
	benchReconcile(filepath.Dir(base))
	busy := benchWaitQuiet()
	m.UnderLoad = busy
	m.Status, m.Start = "running", time.Now().UTC()
	if paths.WriteJSON(base+".marker", m) != nil {
		return
	}
	code := benchMeasure(base, job, lock, busy)
	if benchComplete(base, m, code) != nil {
		return
	}
	in := core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"harness_bench":{"result":%q,"sha":%q}}`,
		job.SessionID, base+".json", job.SHA)))
	benchFinish(in)
}

func benchWaitQuiet() bool {
	busy := benchBusy()
	for polls := 0; busy && polls < benchMaxBusyPolls; polls++ {
		time.Sleep(benchPollWait)
		busy = benchBusy()
	}
	return busy
}

var benchBusy = func() bool {
	running := 0
	status, err := serve.QueryStatus(time.Second)
	if err != nil && !errors.Is(err, serve.ErrNotRunning) {
		return true
	}
	if err == nil {
		for _, pool := range status.Pools {
			running += pool.Running
		}
	}
	quiet, _, err := proc.QuietMachine(context.Background(), running)
	if err != nil || !quiet {
		return true
	}
	entries, _ := os.ReadDir(inflight.Dir())
	for _, entry := range entries {
		if pid, ok := inflight.Owner(entry.Name()); ok && proc.Alive(pid) {
			return true
		}
	}
	return false
}

func benchMeasure(base string, job benchJob, lock *os.File, underLoad bool) int {
	bin, err := paths.SelfBinary()
	if err != nil {
		return -1
	}
	before := benchBaselines(job)
	top, sha, files := job.Top, job.SHA, job.Files
	args := []string{"-c", benchScript, "sh", bin, base + ".json", top, sha}
	for _, p := range files {
		rel, err := filepath.Rel(top, p)
		if err != nil {
			rel = p
		}
		args = append(args, "--path", rel)
	}
	code := benchRun(args, top, lock)
	if code != 0 {
		return code
	}
	return benchAnnotate(base, before, underLoad)
}

// HEAD may have advanced past sha when the asynchronous commit hook runs.
func primaryTrunkHead(ctx context.Context, top, sha string) string {
	out, err := proc.RunGit(ctx, top, "rev-parse", sha+"^{commit}", "--git-dir", "--git-common-dir")
	rows := strings.Fields(out)
	if err != nil || len(rows) != 3 || !strings.HasPrefix(rows[0], sha) || rows[1] != rows[2] {
		return ""
	}
	branch, err := proc.RunGit(ctx, top, "symbolic-ref", "--short", "HEAD")
	if b := strings.TrimSpace(branch); err != nil || b != "main" && b != "master" {
		return ""
	}
	if _, err := proc.RunGit(ctx, top, "merge-base", "--is-ancestor", rows[0], "HEAD"); err != nil {
		return ""
	}
	return rows[0]
}

// benchedFiles are the commit's changed Go files whose package dir holds a Benchmark func.
func benchedFiles(ctx context.Context, top, sha string) []string {
	out, err := proc.RunGit(ctx, top, "show", "--name-only", "--no-renames", "--format=", sha)
	if err != nil {
		return nil
	}
	known := map[string]bool{}
	var files []string
	for n := range strings.SplitSeq(out, "\n") {
		n = strings.TrimSpace(n)
		if !strings.HasSuffix(n, ".go") {
			continue
		}
		dir := filepath.Join(top, filepath.Dir(n))
		ok, seen := known[dir]
		if !seen {
			ok = dirHasBenchmarks(dir)
			known[dir] = ok
		}
		if ok {
			files = append(files, filepath.Join(top, n))
		}
	}
	return files
}

func dirHasBenchmarks(dir string) bool {
	tests, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	return slices.ContainsFunc(tests, func(f string) bool {
		b, err := os.ReadFile(f)
		return err == nil && benchFuncRe.Match(b)
	})
}

type benchDelta struct {
	Change      string `json:"change"`
	P           string `json:"p"`
	Significant bool   `json:"significant"`
}

type benchReport struct {
	Commit   string         `json:"commit"`
	CPUs     int            `json:"cpus"`
	NoisyRun bool           `json:"noisy_load"`
	Packages []benchPackage `json:"packages"`
}

type benchPackage struct {
	Pkg      string        `json:"pkg"`
	Compared bool          `json:"compared"`
	Missing  []string      `json:"missing"`
	Before   benchBaseline `json:"hook_baseline"`
	Stats    struct {
		LoadBefore float64 `json:"load_before"`
		LoadAfter  float64 `json:"load_after"`
	} `json:"stats"`
	Benchmarks []struct {
		Name   string                `json:"name"`
		Flags  []string              `json:"flags"`
		Deltas map[string]benchDelta `json:"deltas"`
	} `json:"benchmarks"`
}

type benchBaseline struct {
	Commit string            `json:"commit"`
	Reason string            `json:"reason,omitempty"`
	Flags  map[string]string `json:"flags"`
}

func benchBaselines(job benchJob) map[string]benchBaseline {
	out := map[string]benchBaseline{}
	for _, file := range job.Files {
		rel, err := filepath.Rel(job.Top, filepath.Dir(file))
		if err != nil {
			continue
		}
		pkg := filepath.ToSlash(rel)
		if _, seen := out[pkg]; seen {
			continue
		}
		name := strings.ReplaceAll(strings.TrimPrefix(pkg, "internal/"), "/", "-")
		if pkg == "." {
			name = "root"
		}
		raw, err := os.ReadFile(filepath.Join(paths.StateDir(), "bench", name, "rolling.txt"))
		if err != nil {
			out[pkg] = benchBaseline{Commit: "none", Reason: "baseline unavailable"}
			continue
		}
		out[pkg] = benchBaselineText(string(raw))
	}
	return out
}

func benchBaselineText(text string) benchBaseline {
	b := benchBaseline{Commit: "unknown", Flags: map[string]string{}}
	values := map[string][]float64{}
	headers := map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(strings.TrimPrefix(line, "# "))
		if len(f) < 2 {
			continue
		}
		headers[f[0]] = f[1]
		benchSample(values, f)
	}
	if headers["commit:"] != "" {
		b.Commit = headers["commit:"]
	}
	b.Reason = benchBaselineLoad(headers)
	for name, samples := range values {
		b.Flags[name] = benchSampleReason(samples)
	}
	return b
}

func benchBaselineLoad(headers map[string]string) string {
	values := map[string]float64{}
	for _, key := range []string{"cpus:", "load-before:", "load-after:"} {
		v, err := strconv.ParseFloat(headers[key], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return "baseline metadata unavailable"
		}
		values[key] = v
	}
	if values["cpus:"] == 0 || headers["commit:"] == "" {
		return "baseline metadata unavailable"
	}
	if max(values["load-before:"], values["load-after:"]) > values["cpus:"]/2 {
		return "baseline load"
	}
	return ""
}

func benchSample(values map[string][]float64, fields []string) {
	if !strings.HasPrefix(fields[0], "Benchmark") {
		return
	}
	for i := 3; i < len(fields); i += 2 {
		if v, err := strconv.ParseFloat(fields[i-1], 64); fields[i] == "ns/op" && err == nil {
			values[fields[0]] = append(values[fields[0]], v)
		}
	}
}

func benchSampleReason(samples []float64) string {
	if len(samples) < 2 {
		return "baseline insufficient samples"
	}
	mean := 0.0
	for _, v := range samples {
		mean += v / float64(len(samples))
	}
	variance := 0.0
	for _, v := range samples {
		variance += (v - mean) * (v - mean) / float64(len(samples)-1)
	}
	if mean <= 0 || math.IsNaN(mean) || math.IsInf(mean, 0) {
		return "baseline invalid samples"
	}
	cv := math.Sqrt(variance) / mean * benchPercent
	if math.Round(cv*benchPercent)/benchPercent > benchQuietCV {
		return "baseline noisy"
	}
	return ""
}

func benchAnnotate(base string, before map[string]benchBaseline, underLoad bool) int {
	raw, err := os.ReadFile(base + ".json")
	if err != nil {
		return -1
	}
	// Preserve the runner's full evidence, including fields this hook does not interpret.
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return -1
	}
	if underLoad {
		document["noisy_load"] = json.RawMessage("true")
	}
	var packages []map[string]json.RawMessage
	if json.Unmarshal(document["packages"], &packages) != nil {
		return -1
	}
	for i := range packages {
		var pkg string
		if json.Unmarshal(packages[i]["pkg"], &pkg) != nil {
			return -1
		}
		baseline := before[pkg]
		if baseline.Commit == "" {
			baseline = benchBaseline{Commit: "unknown", Reason: "baseline metadata unavailable"}
		}
		packages[i]["hook_baseline"], _ = json.Marshal(baseline)
	}
	document["packages"], _ = json.Marshal(packages)
	if paths.WriteJSON(base+".json", document) != nil {
		return -1
	}
	return 0
}

// TickOrBench is the docs-plan-tick module: a finished bench run's payload posts its notices, else Tick.
func TickOrBench(in *core.Input) core.Result {
	if in.ToolName == "Bash" && in.Nested(benchField, "result") != "" {
		return benchFinish(in)
	}
	return Tick(in)
}

// benchFinish turns the finished run's result file into notices on the committing session's next prompt.
func benchFinish(in *core.Input) core.Result {
	result := in.Nested(benchField, "result")
	base := strings.TrimSuffix(result, ".json")
	markerRaw, err := os.ReadFile(base + ".marker")
	var m benchMarker
	if err != nil || json.Unmarshal(markerRaw, &m) != nil || m.Exit == nil || in.SessionID == "" {
		return core.Result{}
	}
	raw, _ := os.ReadFile(result)
	lines := benchNoticeLines(raw)
	if len(lines) > benchMaxLines {
		lines = append(lines[:benchMaxLines-1], benchNoticeSummary(raw, len(lines)-benchMaxLines+1))
	}
	if *m.Exit != 0 {
		lines = []string{fmt.Sprintf("bench %s vs unknown: inconclusive (run failed, exit %d)", in.Nested(benchField, "sha"), *m.Exit)}
	}
	if len(lines) == 0 {
		lines = []string{fmt.Sprintf("bench %s vs unknown: inconclusive (result unavailable or empty); rerun wanted", in.Nested(benchField, "sha"))}
	}
	m.Notice = "delivered"
	for _, line := range lines {
		quoted := "'" + strings.ReplaceAll(base, "'", "'\\''") + "'"
		line += fmt.Sprintf("; result %s; rerun: claude-code %s %s", result, benchWorkerArg, quoted)
		if err := worktree.Append(in.SessionID, worktree.Notice{TS: time.Now().Unix(), From: "bench", Kind: "bench", Text: line}); err != nil {
			m.Notice = "failed: " + err.Error()
		}
	}
	_ = paths.WriteJSON(base+".marker", m)
	return core.Result{}
}

var benchSuffix = regexp.MustCompile(`-\d+$`)

// Every comparison retains its own provenance and reason, including missing comparisons.
func benchNoticeLines(raw []byte) []string {
	var rep benchReport
	if json.Unmarshal(raw, &rep) != nil {
		return nil
	}
	var lines []string
	for _, p := range rep.Packages {
		baseline := p.Before.Commit
		if baseline == "" {
			baseline = "unknown"
		}
		prefix := fmt.Sprintf("bench %s vs %s %s: ", rep.Commit, baseline, p.Pkg)
		for _, b := range p.Benchmarks {
			reason := p.baselineReason(b.Name)
			flags := slices.Clone(b.Flags)
			if reason != "" {
				flags = append(flags, reason)
			}
			loaded := rep.NoisyRun || rep.CPUs > 0 && max(p.Stats.LoadBefore, p.Stats.LoadAfter) > float64(rep.CPUs)/2
			parts := benchParts(loaded, flags, b.Deltas)
			name := benchSuffix.ReplaceAllString(b.Name, "")
			lines = append(lines, prefix+name+" "+strings.Join(parts, "; "))
		}
		for _, missing := range p.Missing {
			lines = append(lines, prefix+"inconclusive ("+missing+"); rerun wanted")
		}
	}
	slices.SortStableFunc(lines, func(a, b string) int { return benchNoticeRank(a) - benchNoticeRank(b) })
	return lines
}

func benchNoticeRank(line string) int {
	switch {
	case strings.Contains(line, "inconclusive"):
		return 0
	case strings.Contains(line, "ns +"), strings.Contains(line, "allocs +"), strings.Contains(line, "B +"):
		return 1
	case strings.Contains(line, "no significant change"):
		return 3
	default:
		return 2
	}
}

func benchNoticeSummary(raw []byte, remaining int) string {
	var rep benchReport
	_ = json.Unmarshal(raw, &rep)
	var baselines []string
	for _, p := range rep.Packages {
		baseline := p.Before.Commit
		if baseline == "" {
			baseline = "unknown"
		}
		if !slices.Contains(baselines, baseline) {
			baselines = append(baselines, baseline)
		}
	}
	return fmt.Sprintf("bench %s vs %s: %d more comparisons; full evidence in result", rep.Commit, strings.Join(baselines, ","), remaining)
}

func (p benchPackage) baselineReason(name string) string {
	if !p.Compared {
		return "no comparable baseline"
	}
	if p.Before.Reason != "" {
		return p.Before.Reason
	}
	if reason, ok := p.Before.Flags[name]; ok {
		return reason
	}
	return "baseline sample unavailable"
}

func benchQuietReason(noisyRun bool, flags []string) string {
	if noisyRun || slices.Contains(flags, "load") {
		return "load"
	}
	if slices.Contains(flags, "noisy") {
		return "noisy"
	}
	for _, flag := range flags {
		if strings.HasPrefix(flag, "baseline ") || flag == "no comparable baseline" {
			return flag
		}
	}
	return ""
}

// The same evidence gate covers every metric and both directions.
func benchParts(noisyRun bool, flags []string, deltas map[string]benchDelta) []string {
	if reason := benchQuietReason(noisyRun, flags); reason != "" {
		return []string{"inconclusive (" + reason + "); rerun wanted"}
	}
	if len(deltas) == 0 {
		return []string{"inconclusive (missing comparison); rerun wanted"}
	}
	var parts []string
	for _, u := range []struct{ unit, label string }{{"allocs/op", "allocs"}, {"B/op", "B"}, {"sec/op", "ns"}} {
		d, ok := deltas[u.unit]
		if !ok || !d.Significant {
			continue
		}
		pv, _, _ := strings.Cut(d.P, " ")
		parts = append(parts, fmt.Sprintf("%s %s (%s)", u.label, d.Change, pv))
	}
	if len(parts) == 0 {
		parts = append(parts, "not judged (no significant change)")
	}
	return parts
}
