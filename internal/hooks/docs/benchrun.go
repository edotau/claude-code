package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	benchCount    = "8" // benchstat needs at least 6 samples per side for a confidence interval
	benchMaxLines = 4
	benchField    = "harness_bench"
)

// benchSpawn starts the detached bench run; a seam so tests spawn nothing.
var benchSpawn = func(args []string, dir string) int { return proc.SpawnDetachedPID("sh", args, dir, nil) }

// benchLoad is the 1-minute load average; a seam for tests.
var benchLoad = func() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		var out []byte
		if out, err = exec.CommandContext(context.Background(), "sysctl", "-n", "vm.loadavg").Output(); err != nil {
			return 0
		}
		raw = out
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(raw)), "{}"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// benchScript runs the bench at low priority, then hands the result back to this module for the notice.
const benchScript = `b=$1; r=$2; p=$3; shift 3
nice -n 19 "$b" review bench "$@" --count ` + benchCount + ` --format json >"$r" 2>"$r.err"
"$b" hook PostToolUse --docs-plan-tick <"$p" >/dev/null 2>&1`

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
		benchStart(in, top, sha, files)
	}
}

// benchStart records the run's marker, applies the busy skip and spawns the detached run.
func benchStart(in *core.Input, top, sha string, files []string) {
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	if os.MkdirAll(state, 0o700) != nil {
		return
	}
	plans.PruneVerify(state, "*")
	base := filepath.Join(state, sha[:12])
	f, err := os.OpenFile(base+".marker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil { // a run for this commit already ran or runs
		return
	}
	defer f.Close()
	if load := benchLoad(); load > float64(runtime.NumCPU())/2 {
		fmt.Fprintf(f, "skipped: busy (load %.2f)\n", load)
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "Bash", "session_id": in.SessionID,
		benchField: map[string]string{"result": base + ".json", "sha": sha},
	})
	bin, err := paths.SelfBinary()
	if err != nil || os.WriteFile(base+".in.json", payload, 0o600) != nil {
		fmt.Fprintln(f, "skipped: no spawn")
		return
	}
	args := []string{"-c", benchScript, "sh", bin, base + ".json", base + ".in.json"}
	for _, p := range files {
		args = append(args, "--path", p)
	}
	if benchSpawn(args, top) == 0 {
		fmt.Fprintln(f, "skipped: spawn failed")
		return
	}
	fmt.Fprintln(f, "running")
}

// primaryTrunkHead is sha's full id when top is the primary checkout on main or master and HEAD holds sha; else "".
// HEAD may be ahead: the async hook runs after later commits land.
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
	NoisyRun bool `json:"noisy_load"`
	Packages []struct {
		Pkg        string `json:"pkg"`
		Benchmarks []struct {
			Name   string                `json:"name"`
			Flags  []string              `json:"flags"`
			Deltas map[string]benchDelta `json:"deltas"`
		} `json:"benchmarks"`
	} `json:"packages"`
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
	raw, err := os.ReadFile(in.Nested(benchField, "result"))
	if err != nil || in.SessionID == "" {
		return core.Result{}
	}
	for _, line := range benchNoticeLines(raw) {
		_ = worktree.Append(in.SessionID, worktree.Notice{TS: time.Now().Unix(), From: "bench", Kind: "bench", Text: line})
	}
	return core.Result{}
}

var benchSuffix = regexp.MustCompile(`-\d+$`)

// benchNoticeLines is one line per moved benchmark, at most benchMaxLines.
func benchNoticeLines(raw []byte) []string {
	var rep benchReport
	if json.Unmarshal(raw, &rep) != nil {
		return nil
	}
	var lines []string
	for _, p := range rep.Packages {
		for _, b := range p.Benchmarks {
			if parts := benchParts(rep.NoisyRun, b.Flags, b.Deltas); len(parts) > 0 {
				name := benchSuffix.ReplaceAllString(b.Name, "")
				lines = append(lines, fmt.Sprintf("bench %s: %s %s", p.Pkg, name, strings.Join(parts, "; ")))
			}
		}
	}
	if len(lines) > benchMaxLines {
		lines = append(lines[:benchMaxLines-1], fmt.Sprintf("bench: %d more moved; see state/harness/bench", len(lines)-benchMaxLines+1))
	}
	return lines
}

// benchParts reports allocs and B moves always, and ns only when the run and the benchmark were quiet.
func benchParts(noisyRun bool, flags []string, deltas map[string]benchDelta) []string {
	var parts []string
	for _, u := range []struct{ unit, label string }{{"allocs/op", "allocs"}, {"B/op", "B"}, {"sec/op", "ns"}} {
		d, ok := deltas[u.unit]
		if !ok || !d.Significant {
			continue
		}
		switch {
		case u.unit == "sec/op" && noisyRun:
			parts = append(parts, "ns not judged (load)")
		case u.unit == "sec/op" && slices.Contains(flags, "noisy"):
			parts = append(parts, "ns not judged (noisy)")
		default:
			pv, _, _ := strings.Cut(d.P, " ")
			parts = append(parts, fmt.Sprintf("%s %s (%s)", u.label, d.Change, pv))
		}
	}
	return parts
}
