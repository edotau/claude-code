package bench

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pendingFixture(t *testing.T, pkg string) (string, string) {
	t.Helper()
	top := t.TempDir()
	dir := filepath.Join(top, pkg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "package p\nimport \"testing\"\nfunc BenchmarkA(b *testing.B) {}\n"
	if err := os.WriteFile(filepath.Join(dir, "bench_test.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	epochs, err := benchmarkEpochs(top, pkg)
	if err != nil {
		t.Fatal(err)
	}
	after := filepath.Join(top, "after.txt")
	if err := os.WriteFile(after, []byte(epochHeader(epochs)+"BenchmarkA-8 1 10 ns/op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return top, after
}

func TestApplyPending(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "applied", true: "dropped"}[stale], func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			top, after := pendingFixture(t, "internal/p")
			for _, lane := range []string{"bench-p", "other"} {
				if err := recordPending(loopOpts{pkg: "internal/p", lane: lane, sha: "abc", after: after}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if stale {
				file := filepath.Join(top, "internal/p/bench_test.go")
				if err := os.WriteFile(file, []byte("package p\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApplyPending(top, "bench-p"); err != nil {
				t.Fatal(err)
			}
			checkPendingApplied(t, top, after, stale)
		})
	}
}

func checkPendingApplied(t *testing.T, top, after string, stale bool) {
	t.Helper()
	state, err := readPending()
	want := "applied"
	if stale {
		want = "dropped"
	}
	if err != nil || len(state.Rows) != 2 {
		t.Fatalf("state = %+v, %v", state, err)
	}
	if got := state.Rows[0].Status + "," + state.Rows[1].Status; got != want+",pending" {
		t.Fatalf("statuses = %s", got)
	}
	checkPendingBaseline(t, after, stale)
	checkPendingRepeat(t, top)
}

func checkPendingRepeat(t *testing.T, top string) {
	t.Helper()
	before, err := os.Stat(pendingFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyPending(top, "bench-p"); err != nil {
		t.Fatal(err)
	}
	next, err := os.Stat(pendingFile())
	if err != nil || !os.SameFile(before, next) || before.ModTime() != next.ModTime() {
		t.Fatalf("repeat rewrote state: %v", err)
	}
}

func checkPendingBaseline(t *testing.T, after string, stale bool) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(benchRoot(), "p", "rolling.txt"))
	if stale {
		if !os.IsNotExist(err) {
			t.Fatalf("stale baseline applied: %q, %v", got, err)
		}
		return
	}
	body, readErr := os.ReadFile(after)
	if err != nil || readErr != nil || string(body) != string(got) {
		t.Fatalf("baseline differs: %q, %v, %v", got, err, readErr)
	}
}

func TestPendingCLI(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, after := pendingFixture(t, "internal/p")
	for _, lane := range []string{"bench-p", "other"} {
		args := []string{"--record", "--verdict", "kept", "--pkg", "internal/p", "--what", "speed",
			"--lane", lane, "--after", after, "--sha", "abc"}
		if code := RunBench(args, &strings.Builder{}, &strings.Builder{}); code != 0 {
			t.Fatalf("record exit %d", code)
		}
	}
	var out, stderr strings.Builder
	if code := RunBench([]string{"--pending", "--lane", "bench-p", "--format", "json"}, &out, &stderr); code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr.String())
	}
	var rows []pendingRow
	if err := json.Unmarshal([]byte(out.String()), &rows); err != nil || len(rows) != 1 || rows[0].Lane != "bench-p" {
		t.Fatalf("pending list = %s, %v", out.String(), err)
	}
}

func TestPendingOrphaned(t *testing.T) {
	top := gitRepo(t)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", top}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	head := git("rev-parse", "HEAD")
	dangling := git("-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", "HEAD^{tree}", "-m", "lost")
	if orphaned(top, pendingRow{Lane: "gone", Sha: head}) {
		t.Error("a sha on a branch was flagged")
	}
	if !orphaned(top, pendingRow{Lane: "gone", Sha: dangling}) {
		t.Error("a sha on no branch with its lane gone was not flagged")
	}
	git("branch", "worktree-kept", head)
	if orphaned(top, pendingRow{Lane: "kept", Sha: dangling}) {
		t.Error("a row whose lane branch exists was flagged")
	}
}

func TestPendingFlagOrder(t *testing.T) {
	for _, args := range [][]string{{"--pending", "--record=false"}, {"--record=false", "--pending"}} {
		o, _, done := parseArgs(args, &strings.Builder{}, &strings.Builder{})
		if done || !o.loop.pending || !o.loop.record {
			t.Fatalf("%v did not select pending reporting: %+v", args, o.loop)
		}
	}
}

func TestRecordPendingRequiresLaneAndEpochs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, after := pendingFixture(t, "internal/p")
	l := loopOpts{pkg: "internal/p", what: "speed", verdict: "kept", after: after, sha: "abc"}
	if err := recordRow(l); err == nil {
		t.Fatal("accepted kept benchmark without lane")
	}
	l.lane = "bench-p"
	if err := os.WriteFile(after, []byte("BenchmarkA-8 1 10 ns/op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordRow(l); err == nil {
		t.Fatal("accepted pending baseline without epoch")
	}
	if _, err := os.Stat(pendingFile()); !os.IsNotExist(err) {
		t.Fatalf("invalid record wrote pending state: %v", err)
	}
}
