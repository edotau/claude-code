package docs

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

const cannedBench = `{"commit":"abc1234","cpus":8,"noisy_load":false,"packages":[{
"pkg":"internal/memory","compared":true,
"hook_baseline":{"commit":"def5678","flags":{"PromptRecall/bank-8":"","Noisy-8":"","Still-8":""}},
"benchmarks":[
{"name":"PromptRecall/bank-8","deltas":{"allocs/op":{"change":"-12.00%","p":"p=0.000 n=5","significant":true},
"sec/op":{"change":"-3.00%","p":"p=0.010 n=5","significant":true}}},
{"name":"Noisy-8","flags":["noisy"],"deltas":{"sec/op":{"change":"+9.00%","p":"p=0.020","significant":true}}},
{"name":"Still-8","deltas":{"sec/op":{"change":"~","p":"p=1.000","significant":false}}}]}]}`

func TestBenchNoticeLines(t *testing.T) {
	got := benchNoticeLines([]byte(cannedBench))
	want := []string{
		"bench abc1234 vs def5678 internal/memory: Noisy inconclusive (noisy); rerun wanted",
		"bench abc1234 vs def5678 internal/memory: PromptRecall/bank allocs -12.00% (p=0.000); ns -3.00% (p=0.010)",
		"bench abc1234 vs def5678 internal/memory: Still not judged (no significant change)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for _, change := range []struct{ old, new, reason string }{
		{`"noisy_load":false`, `"noisy_load":true`, "load"},
		{`"pkg":"internal/memory"`, `"pkg":"internal/memory","stats":{"load_after":9}`, "load"},
		{`"PromptRecall/bank-8":""`, `"PromptRecall/bank-8":"baseline noisy"`, "baseline noisy"},
		{`"PromptRecall/bank-8":""`, `"PromptRecall/bank-8":"baseline load"`, "baseline load"},
		{`"compared":true`, `"compared":false`, "no comparable baseline"},
		{`"name":"PromptRecall/bank-8"`, `"name":"PromptRecall/bank-8","flags":["load"]`, "load"},
	} {
		raw := strings.Replace(cannedBench, change.old, change.new, 1)
		for _, direction := range []string{"-3.00%", "+3.00%"} {
			raw = strings.ReplaceAll(raw, "-3.00%", direction)
			line := noticeFor(t, raw, "PromptRecall/bank")
			if !strings.Contains(line, "inconclusive ("+change.reason+")") || strings.Contains(line, "%") {
				t.Errorf("untrusted %s comparison judged: %s", direction, line)
			}
		}
	}
}

func noticeFor(t *testing.T, raw, name string) string {
	t.Helper()
	for _, line := range benchNoticeLines([]byte(raw)) {
		if strings.Contains(line, name) {
			return line
		}
	}
	t.Fatalf("no notice for %s", name)
	return ""
}

func benchRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"x.go": "package pkg\n", "x_test.go": "package pkg\nimport \"testing\"\nfunc BenchmarkX(b *testing.B) {}\n"} {
		if err := os.WriteFile(filepath.Join(pkg, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "pkg"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return dir, strings.TrimSpace(string(out))
}

func TestBenchOnCommitQueuesBusyAndDeduplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, sha := benchRepo(t)
	spawn := benchSpawn
	t.Cleanup(func() { benchSpawn = spawn })
	var spawned int
	benchSpawn = func(args []string, _ string) int {
		spawned++
		if args[len(args)-1] != filepath.Join(paths.StateDir(), "bench", "post-commit", sha) {
			t.Errorf("spawn lost commit identity: %v", args)
		}
		return 1
	}
	in := core.ParseInput(strings.NewReader(`{"session_id":"s1","tool_name":"Bash"}`))
	leaf := core.ParseInput(strings.NewReader(`{"agent_id":"a1","tool_name":"Bash"}`))
	benchOnCommit(context.Background(), leaf, dir, sha)
	benchOnCommit(context.Background(), in, dir, sha[:7])
	benchOnCommit(context.Background(), in, dir, sha[:7])
	if spawned != 1 {
		t.Fatalf("spawned %d, want one queued worker", spawned)
	}
	m := readBenchMarker(t, filepath.Join(paths.StateDir(), "bench", "post-commit", sha))
	if m.Status != "queued" || m.Exit != nil {
		t.Fatalf("busy commit was not queued: %+v", m)
	}
}

func readBenchMarker(t *testing.T, base string) benchMarker {
	t.Helper()
	raw, err := os.ReadFile(base + ".marker")
	var m benchMarker
	if err != nil || json.Unmarshal(raw, &m) != nil {
		t.Fatalf("marker: %s %v", raw, err)
	}
	return m
}

func TestBenchWorkerSerializesAndFinishesBeforeNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix locks")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	entered, release, admitted := stubBenchWorkers(t)
	in := core.ParseInput(strings.NewReader(`{"session_id":"bench-serial-test"}`))
	var bases []string
	for _, sha := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
		benchStart(context.Background(), in, t.TempDir(), sha, nil)
		bases = append(bases, filepath.Join(paths.StateDir(), "bench", "post-commit", sha))
	}
	done := make(chan struct{}, 2)
	for _, base := range bases {
		go func() { benchWorker(base); done <- struct{}{} }()
	}
	assertBenchWaiting(t, entered, "ran while busy")
	admitted.Store(true)
	first := waitBench(t, entered)
	assertBenchWaiting(t, entered, "two measurements overlapped")
	release <- struct{}{}
	second := waitBench(t, entered)
	if first == second {
		t.Fatal("lost queued commit")
	}
	release <- struct{}{}
	for range bases {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("worker did not finish")
		}
	}
	for _, base := range bases {
		assertBenchComplete(t, base)
	}
}

func TestBenchWorkerMeasuresAfterBusyBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix locks")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	entered, release, _ := stubBenchWorkers(t)
	limit := benchMaxBusyPolls
	t.Cleanup(func() { benchMaxBusyPolls = limit })
	benchMaxBusyPolls = 2
	checks := 0
	benchBusy = func() bool { checks++; return true }
	sha := strings.Repeat("c", 40)
	base := filepath.Join(paths.StateDir(), "bench", "post-commit", sha)
	in := core.ParseInput(strings.NewReader(`{"session_id":"bench-busy-bound"}`))
	benchStart(context.Background(), in, t.TempDir(), sha, nil)
	done := make(chan struct{})
	go func() { benchWorker(base); close(done) }()
	if got := waitBench(t, entered); got != base+".json" {
		t.Errorf("run result = %q", got)
	}
	release <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("busy worker did not finish")
	}
	assertBusyBenchRun(t, base, checks)
}

func assertBusyBenchRun(t *testing.T, base string, checks int) {
	t.Helper()
	if checks != 3 {
		t.Fatalf("busy checks = %d, want initial plus two retries", checks)
	}
	m := readBenchMarker(t, base)
	if m.Status != "done" || !m.UnderLoad {
		t.Fatalf("busy run marker = %+v", m)
	}
	raw, err := os.ReadFile(base + ".json")
	var rep benchReport
	if err != nil || json.Unmarshal(raw, &rep) != nil || !rep.NoisyRun {
		t.Fatalf("busy run lacks noisy_load: %s (%v)", raw, err)
	}
	if line := noticeFor(t, string(raw), "PromptRecall/bank"); !strings.Contains(line, "inconclusive (load)") {
		t.Fatalf("loaded run was judged: %s", line)
	}
}

func assertBenchWaiting(t *testing.T, ch <-chan string, message string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(message)
	case <-time.After(5 * time.Millisecond):
	}
}

func stubBenchWorkers(t *testing.T) (chan string, chan struct{}, *atomic.Bool) {
	t.Helper()
	spawn, run, busy, poll := benchSpawn, benchRun, benchBusy, benchPollWait
	t.Cleanup(func() { benchSpawn, benchRun, benchBusy, benchPollWait = spawn, run, busy, poll })
	benchSpawn = func([]string, string) int { return 1 }
	admitted := new(atomic.Bool)
	benchBusy = func() bool { return !admitted.Load() }
	benchPollWait = time.Millisecond
	entered, release := make(chan string, 2), make(chan struct{})
	var active atomic.Int32
	benchRun = func(args []string, _ string, _ *os.File) int {
		if active.Add(1) != 1 {
			t.Error("overlapping benchmark measurements")
		}
		defer active.Add(-1)
		entered <- args[4]
		<-release
		if err := os.WriteFile(args[4], []byte(cannedBench), 0o600); err != nil {
			t.Error(err)
		}
		return 0
	}
	return entered, release, admitted
}

func assertBenchComplete(t *testing.T, base string) {
	t.Helper()
	m := readBenchMarker(t, base)
	if m.Status != "done" || m.PID != os.Getpid() || m.Start.IsZero() || m.End.Before(m.Start) {
		t.Errorf("incomplete lifecycle: %+v", m)
	}
	if m.Exit == nil || *m.Exit != 0 || m.Notice != "delivered" {
		t.Errorf("incomplete outcome: %+v", m)
	}
}

func waitBench(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("queued benchmark never entered runner")
		return ""
	}
}

func TestBenchReconcile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	for _, status := range []string{"queued", "running"} {
		base := filepath.Join(dir, status)
		m := benchMarker{Status: status, PID: 1 << 30, Start: time.Now().Add(-time.Minute)}
		if err := paths.WriteJSON(base+".marker", m); err != nil {
			t.Fatal(err)
		}
		benchReconcile(dir)
		m = readBenchMarker(t, base)
		if m.Status != "failed" || m.Exit == nil || *m.Exit != -1 || m.End.IsZero() {
			t.Fatalf("dead process left active: %+v", m)
		}
	}
	if (benchMarker{Status: "running", PID: os.Getpid()}).abandoned() {
		t.Fatal("live worker treated as abandoned")
	}
}

func TestBenchFinishRequiresCompletedMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := filepath.Join(t.TempDir(), "failure")
	in := core.ParseInput(strings.NewReader(`{"session_id":"failed-bench","harness_bench":{"result":"` + base + `.json","sha":"abc"}}`))
	if err := paths.WriteJSON(base+".marker", benchMarker{Status: "running", PID: os.Getpid(), Start: time.Now()}); err != nil {
		t.Fatal(err)
	}
	benchFinish(in)
	if notices := worktree.PeekNotices("failed-bench", "bench"); len(notices) != 0 {
		t.Fatalf("notice before final marker: %+v", notices)
	}
	if err := benchComplete(base, benchMarker{PID: os.Getpid(), Start: time.Now()}, 3); err != nil {
		t.Fatal(err)
	}
	benchFinish(in)
	m := readBenchMarker(t, base)
	if m.Status != "failed" || *m.Exit != 3 || m.Notice != "delivered" {
		t.Fatalf("notification changed run outcome: %+v", m)
	}
	notices := worktree.PeekNotices("failed-bench", "bench")
	if len(notices) != 1 || !strings.Contains(notices[0].Text, "bench abc vs unknown: inconclusive (run failed, exit 3)") || !strings.Contains(notices[0].Text, "rerun: claude-code "+benchWorkerArg) {
		t.Fatalf("failure notice lacks evidence/rerun: %+v", notices)
	}
}

func TestBenchBaselineEvidence(t *testing.T) {
	const quiet = "# commit: old1234\n# cpus: 8\n# load-before: 1\n# load-after: 2\nBenchmarkX-8 1 10 ns/op\nBenchmarkX-8 1 10.1 ns/op\n"
	for _, tc := range []struct{ name, text, reason string }{
		{"quiet", quiet, ""},
		{"noisy", strings.Replace(quiet, "10.1 ns/op", "15 ns/op", 1), "baseline noisy"},
		{"loaded", strings.Replace(quiet, "load-after: 2", "load-after: 9", 1), "baseline load"},
		{"missing load", strings.Replace(quiet, "# load-after: 2\n", "", 1), "baseline metadata unavailable"},
		{"unknown", "BenchmarkX-8 1 10 ns/op\n", "baseline metadata unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := benchBaselineText(tc.text)
			reason := b.Reason
			if reason == "" {
				reason = b.Flags["BenchmarkX-8"]
			}
			if reason != tc.reason {
				t.Fatalf("baseline = %+v, want %q", b, tc.reason)
			}
		})
	}
}

func TestBenchNoticeFailurePreservesExit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.StateDir(), "notices"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(t.TempDir(), "run")
	if err := benchComplete(base, benchMarker{Start: time.Now()}, 3); err != nil {
		t.Fatal(err)
	}
	in := core.ParseInput(strings.NewReader(`{"session_id":"undeliverable","harness_bench":{"result":"` + base + `.json","sha":"abc"}}`))
	benchFinish(in)
	m := readBenchMarker(t, base)
	if *m.Exit != 3 || m.Status != "failed" || !strings.HasPrefix(m.Notice, "failed: ") {
		t.Fatalf("delivery failure obscured measurement exit: %+v", m)
	}
}

func TestBenchAnnotatePreservesRunnerEvidence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := filepath.Join(paths.StateDir(), "bench", "memory")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	baseline := "# commit: old1234\n# cpus: 8\n# load-before: 1\n# load-after: 2\nBenchmarkX-8 1 10 ns/op\nBenchmarkX-8 1 10.1 ns/op\n"
	writeBenchFixture(t, filepath.Join(root, "rolling.txt"), baseline)
	before := benchBaselines(benchJob{Top: "/repo", Files: []string{"/repo/internal/memory/a.go"}})
	if before["internal/memory"].Commit != "old1234" {
		t.Fatalf("wrong baseline location: %+v", before)
	}
	base := filepath.Join(t.TempDir(), "run")
	raw := strings.Replace(cannedBench, `"pkg":"internal/memory"`, `"pkg":"internal/memory","future_evidence":{"epoch":"keep"}`, 1)
	writeBenchFixture(t, base+".json", raw)
	if code := benchAnnotate(base, before, false); code != 0 {
		t.Fatalf("annotate exit %d", code)
	}
	got, err := os.ReadFile(base + ".json")
	var document struct {
		Packages []struct {
			Before benchBaseline          `json:"hook_baseline"`
			Future struct{ Epoch string } `json:"future_evidence"`
		} `json:"packages"`
	}
	if err != nil || json.Unmarshal(got, &document) != nil || len(document.Packages) != 1 {
		t.Fatalf("invalid annotated document: %s %v", got, err)
	}
	if document.Packages[0].Before.Commit != "old1234" || document.Packages[0].Future.Epoch != "keep" {
		t.Fatalf("lost provenance or runner evidence: %s %v", got, err)
	}
}

func writeBenchFixture(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBenchCommitFormsAndLaggingHead(t *testing.T) {
	for _, cmd := range []string{"rtk git add a && rtk git commit -m x", "git -C /r commit -qm x", "claude-code worktree commit --task p 1 -m x f.go"} {
		if !tickMatch.MatchString(cmd) {
			t.Errorf("tickMatch missed %q", cmd)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, sha := benchRepo(t)
	if out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "y").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var spawned int
	origSpawn := benchSpawn
	t.Cleanup(func() { benchSpawn = origSpawn })
	benchSpawn = func([]string, string) int { spawned++; return 1 }
	in := core.ParseInput(strings.NewReader(`{"session_id":"s1","tool_name":"Bash"}`))
	benchOnCommit(context.Background(), in, dir, sha[:7])
	if spawned != 1 {
		t.Errorf("spawned %d runs for a commit HEAD has moved past, want 1", spawned)
	}
}
