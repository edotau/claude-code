package bench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// gitRepo is a temp repo with one commit holding internal/x/a.go.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "internal/x/a.go"), "package x\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const runFile = "goos: darwin\n" +
	"BenchmarkA-8\t1000\t300 ns/op\t16 B/op\t2 allocs/op\n" +
	"BenchmarkA-8\t1000\t100 ns/op\t16 B/op\t2 allocs/op\n" +
	"BenchmarkA-8\t1000\t200 ns/op\t16 B/op\t2 allocs/op\n" +
	"BenchmarkB-8\t1000\t50 ns/op\t0 B/op\t0 allocs/op\n" +
	"PASS\n"

func TestDriverLineHelpers(t *testing.T) {
	for _, c := range []struct{ in, first, last string }{
		{"", "", ""},
		{"one", "one", "one"},
		{"  a\nb\n\nc  \n\n", "a", "c"},
		{"KEEP\nbecause", "KEEP", "because"},
	} {
		if got := firstLine(c.in); got != c.first {
			t.Errorf("firstLine(%q) = %q, want %q", c.in, got, c.first)
		}
		if got := lastLine(c.in); got != c.last {
			t.Errorf("lastLine(%q) = %q, want %q", c.in, got, c.last)
		}
	}
}

func TestDriverMedians(t *testing.T) {
	f := filepath.Join(t.TempDir(), "run.txt")
	writeFile(t, f, runFile)
	got, err := medians(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BenchmarkA-8: 200.0 ns/op, 16 B/op, 2 allocs/op (3 samples)",
		"BenchmarkB-8: 50.0 ns/op, 0 B/op, 0 allocs/op (1 samples)"} {
		if !strings.Contains(got, want) {
			t.Errorf("medians lacks %q:\n%s", want, got)
		}
	}
	writeFile(t, f, "no benchmarks here\n")
	if _, err := medians(f); err == nil {
		t.Error("a file with no samples was accepted")
	}
}

func TestDriverSavedMSDay(t *testing.T) {
	dir := t.TempDir()
	before, after := filepath.Join(dir, "before"), filepath.Join(dir, "after")
	writeFile(t, before, "BenchmarkA-8\t1\t3000 ns/op\nBenchmarkB-8\t1\t9000 ns/op\nBenchmarkC-8\t1\t5000 ns/op\n")
	writeFile(t, after, "BenchmarkA-8\t1\t1000 ns/op\nBenchmarkB-8\t1\t8000 ns/op\nBenchmarkOther-8\t1\t1 ns/op\n")
	c := candidate{CallsPerDay: 1e6, res: ABResult{Before: before, After: after,
		Decision: Decision{Faster: []string{"A-8", "B-8"}}}}
	if got := savedMSDay(c); got != 2000 { // the largest judged drop is A's 2000 ns, times 1e6 calls
		t.Errorf("savedMSDay = %v, want 2000", got)
	}
	c.res.Decision.Faster = []string{"B-8"} // A's drop is larger but the decision did not judge it faster
	if got := savedMSDay(c); got != 1000 {
		t.Errorf("only B judged: savedMSDay = %v, want 1000", got)
	}
	c.res.Decision.Faster = nil // an allocation-only KEEP saves no judged time
	if got := savedMSDay(c); got != 0 {
		t.Errorf("no judged timing win: savedMSDay = %v, want 0", got)
	}
	c.res.Decision.Faster = []string{"A-8"}
	c.res.After = filepath.Join(dir, "missing")
	if got := savedMSDay(c); got != 0 {
		t.Errorf("unreadable after file: %v, want 0", got)
	}
}

func TestDriverOwnedFilesSkipsTests(t *testing.T) {
	lane := t.TempDir()
	for _, f := range []string{"a.go", "b.go", "a_test.go", "notes.txt"} {
		writeFile(t, filepath.Join(lane, "internal/x", f), "x")
	}
	out, err := ownedFiles(lane, "internal/x")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(lane, "internal/x/a.go"), filepath.Join(lane, "internal/x/b.go")}
	if !slices.Equal(got, want) {
		t.Errorf("owned = %v, want %v", got, want)
	}
}

func TestDriverNextSlugSkipsExistingBranch(t *testing.T) {
	d := &driver{top: gitRepo(t)}
	day := time.Now().Format("20060102")
	first := "bench-hooks-safety-" + day + "-1"
	if got := d.nextSlug("internal/hooks/safety"); got != first {
		t.Fatalf("slug = %q, want %q", got, first)
	}
	if out, err := exec.Command("git", "-C", d.top, "branch", "worktree-"+first).CombinedOutput(); err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	if got, want := d.nextSlug("internal/hooks/safety"), "bench-hooks-safety-"+day+"-2"; got != want {
		t.Errorf("slug = %q, want %q", got, want)
	}
}

func TestDriverGatesRevert(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"outside the package", map[string]string{"internal/x/a.go": "package x // edited\n", "internal/y/b.go": "package y\n"},
			"internal/y/b.go is outside internal/x"},
		{"test file", map[string]string{"internal/x/a_test.go": "package x\n"}, "internal/x/a_test.go is a test file"},
		{"no change", nil, "changed nothing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lane := gitRepo(t)
			for f, body := range c.files {
				writeFile(t, filepath.Join(lane, f), body)
			}
			cand := candidate{Pkg: "internal/x", lane: lane}
			why, err := (&driver{}).gates(context.Background(), &cand)
			if err != nil || !strings.Contains(why, c.want) {
				t.Errorf("gates = %q, %v; want reason containing %q", why, err, c.want)
			}
		})
	}
}

// judgeRun fakes `agents ask|wait|result` with the given answer and logs every other call.
func judgeRun(t *testing.T, answer string) *[]string {
	t.Helper()
	var calls []string
	old := driverRun
	t.Cleanup(func() { driverRun = old })
	driverRun = func(_ context.Context, _, _, _ string, args ...string) (string, int, error) {
		switch {
		case slices.Equal(args[:2], []string{"agents", "ask"}):
			return `{"id":"j1"}` + "\n", 0, nil
		case slices.Equal(args[:2], []string{"agents", "result"}):
			b, _ := json.Marshal(askEnvelope{Status: "ok", Answer: answer})
			return string(b), 0, nil
		}
		calls = append(calls, strings.Join(args, " "))
		return "", 0, nil
	}
	return &calls
}

// wantRows checks the scorecard holds no row (verdict "") or exactly one with that verdict.
func wantRows(t *testing.T, verdict string) {
	t.Helper()
	rows, err := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
	var got []string
	for _, r := range rows {
		got = append(got, r.Verdict)
	}
	if want := []string{verdict}; verdict != "" && !slices.Equal(got, want) || verdict == "" && len(got) != 0 {
		t.Errorf("scorecard verdicts = %v, %v; want %q", got, err, verdict)
	}
}

func TestDriverJudgeVerdicts(t *testing.T) {
	cases := []struct {
		name, answer string
		want         outcome
		cleaned      bool
		rowVerdict   string // "" = nothing recorded
	}{
		{"owner flag holds even after KEEP", "KEEP\n" + ownerFlag + ": touches the guard", held, false, ""},
		{"other first line reverts", "REVERT\nchanges behaviour", reverted, true, "reverted"},
		{"KEEP in prose is not KEEP", "I would KEEP this", reverted, true, "reverted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			calls := judgeRun(t, c.answer)
			lane := gitRepo(t)
			cand := candidate{Pkg: "internal/x", What: "cut allocs", slug: "bench-x-1", lane: lane, parent: "HEAD",
				res: ABResult{Before: "before.txt", After: "after.txt", Decision: Decision{Rows: []string{"A-8 sec/op -20%"}}}}
			d := &driver{top: lane, exe: "claude-code", out: io.Discard, log: io.Discard, skip: map[string]bool{}}
			got, err := d.judge(context.Background(), cand)
			if err != nil || got != c.want {
				t.Fatalf("judge = %v, %v; want %v", got, err, c.want)
			}
			if cleaned := slices.Contains(*calls, "worktree clean --force bench-x-1"); cleaned != c.cleaned {
				t.Errorf("lane cleaned = %v, want %v (calls %v)", cleaned, c.cleaned, *calls)
			}
			if skipped := d.skip[closeKey("internal/x", "cut allocs")]; skipped != (c.want == held) {
				t.Errorf("skip set = %v for outcome %v", skipped, c.want)
			}
			wantRows(t, c.rowVerdict)
		})
	}
}

// stubLoop runs the loop through runDriver with canned steps; it returns the exit code and output.
func stubLoop(t *testing.T, max int, steps loopSteps) (int, string) {
	t.Helper()
	var out strings.Builder
	old := newDriverFn
	t.Cleanup(func() { newDriverFn = old })
	newDriverFn = func(io.Writer, io.Writer) (*driver, error) {
		return &driver{out: &out, log: io.Discard, skip: map[string]bool{}, steps: steps}, nil
	}
	return runDriver(loopOpts{maxItems: max}, &out, io.Discard), out.String()
}

// scripted is steps whose item returns the outcomes in order and counts the calls.
func scripted(outcomes []outcome, busy bool, calls *int) loopSteps {
	return loopSteps{
		quiet: func(context.Context) (bool, string) { return !busy, "load 9.00" },
		pick:  func() (Item, bool, error) { return Item{Pkg: "internal/x", What: "w"}, true, nil },
		item: func(context.Context, Item) (outcome, error) {
			o := outcomes[min(*calls, len(outcomes)-1)]
			*calls++
			return o, nil
		},
	}
}

func TestDriverLoopBudgets(t *testing.T) {
	cases := []struct {
		name     string
		max      int
		outcomes []outcome
		busy     bool
		calls    int
		say      string
	}{
		{"max 0 does nothing", 0, []outcome{kept}, false, 0, "done (--max 0)"},
		{"max caps items", 2, []outcome{kept}, false, 2, "done (--max 2)"},
		{"two reverts stop the loop", 5, []outcome{reverted}, false, 2, "2 reverts in a row; stopping"},
		{"a keep resets the revert count", 4, []outcome{reverted, kept, reverted, kept}, false, 4, "done (--max 4)"},
		{"busy machine defers", 3, []outcome{kept}, true, 0, "machine busy (load 9.00); deferred"},
		{"busy measurement lock defers", 3, []outcome{deferred}, false, 1, "measurement lock busy; deferred"},
		{"an inconclusive run stops", 3, []outcome{noComparison}, false, 1, "no conclusive comparison"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			calls := 0
			code, out := stubLoop(t, c.max, scripted(c.outcomes, c.busy, &calls))
			if code != 0 || calls != c.calls || !strings.Contains(out, c.say) {
				t.Errorf("exit %d, %d items (want %d), output %q (want %q)", code, calls, c.calls, out, c.say)
			}
		})
	}
}

func TestDriverMeasuredExcludesTheWorkerLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "measurement.lock")
	oldPath, oldWait := measureLockPath, measureWait
	t.Cleanup(func() { measureLockPath, measureWait = oldPath, oldWait })
	measureLockPath, measureWait = func() (string, error) { return lock, nil }, 20*time.Millisecond
	ran := false
	held, err := paths.TryFileLock(lock, 0, func() error { // the post-commit worker's flock on the same file
		return measured(func() error { ran = true; return nil })
	})
	if !held || !errors.Is(err, errMeasureBusy) || ran {
		t.Fatalf("under the worker's lock: held=%v err=%v ran=%v, want busy and fn skipped", held, err, ran)
	}
	if err := measured(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("free lock: err=%v ran=%v", err, ran)
	}
}

func TestDriverHeldLockDefers(t *testing.T) {
	calls := 0
	steps := scripted([]outcome{kept}, false, &calls)
	var code int
	var out string
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(benchRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	ran, err := paths.TryFileLock(filepath.Join(benchRoot(), "loop.lock"), 0, func() error {
		code, out = stubLoop(t, 3, steps)
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("test could not take the lock: %v, %v", ran, err)
	}
	if code != 0 || calls != 0 || !strings.Contains(out, "another loop is running; deferred") {
		t.Errorf("exit %d, %d items, output %q", code, calls, out)
	}
}
