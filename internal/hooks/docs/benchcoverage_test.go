package docs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func benchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)
	out, err := exec.CommandContext(context.Background(), "git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// benchCommit writes file in dir's repo and commits it, returning the new sha.
func benchCommit(t *testing.T, dir, file string) string {
	t.Helper()
	writeBenchFixture(t, filepath.Join(dir, file), "package "+filepath.Base(filepath.Dir(file))+"\n// "+file+"\n")
	benchGit(t, dir, "add", file)
	benchGit(t, dir, "commit", "-qm", file)
	return benchGit(t, dir, "rev-parse", "HEAD")
}

// benchLanding is a repo whose trunk moved across a plain, a benched and a merge commit after base.
func benchLanding(t *testing.T) (dir, base, plain, benched, merge string) {
	t.Helper()
	dir, base = benchRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain = benchCommit(t, dir, "doc/d.go")
	benched = benchCommit(t, dir, "pkg/y.go")
	benchGit(t, dir, "checkout", "-qb", "side")
	benchCommit(t, dir, "pkg/z.go")
	benchGit(t, dir, "checkout", "-q", "main")
	benchGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	return dir, base, plain, benched, benchGit(t, dir, "rev-parse", "HEAD")
}

func TestBenchLandedQueuesEveryBenchedCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, base, plain, benched, merge := benchLanding(t)
	spawn := benchSpawn
	t.Cleanup(func() { benchSpawn = spawn })
	spawned := 0
	benchSpawn = func([]string, string) int { spawned++; return 1 }
	BenchLanded(context.Background(), dir, base, merge, "s1")
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	var got []string
	for _, b := range benchQueued(state) {
		got = append(got, filepath.Base(b))
	}
	slices.Sort(got)
	want := []string{benched, merge}
	slices.Sort(want)
	if spawned != 1 || !slices.Equal(got, want) {
		t.Fatalf("spawned %d, queued %v; want one consumer and %v (plain %s skipped)", spawned, got, want, plain)
	}
	BenchLanded(context.Background(), dir, base, merge, "s1") // a repeated landing queues nothing new
	if n := len(benchQueued(state)); n != 2 {
		t.Fatalf("repeat landing queued %d jobs, want 2", n)
	}
	benchGit(t, dir, "checkout", "-qb", "lane")
	BenchLanded(context.Background(), dir, "", benchCommit(t, dir, "pkg/w.go"), "s1")
	if n := len(benchQueued(state)); n != 2 {
		t.Fatalf("a commit off the trunk was queued: %d jobs", n)
	}
	benchGit(t, dir, "checkout", "-q", "main")
	assertLongLandingQueued(t, dir, merge, state)
}

// assertLongLandingQueued lands 21 benched commits then a benchmark deletion: each is judged in its own tree.
func assertLongLandingQueued(t *testing.T, dir, from, state string) {
	t.Helper()
	for i := range 21 {
		benchCommit(t, dir, "pkg/v"+strconv.Itoa(i)+".go")
	}
	benchGit(t, dir, "rm", "-q", "pkg/x_test.go")
	benchGit(t, dir, "commit", "-qm", "drop benchmarks")
	BenchLanded(context.Background(), dir, from, benchGit(t, dir, "rev-parse", "HEAD"), "s1")
	if n := len(benchQueued(state)); n != 2+21 {
		t.Fatalf("long landing queued %d jobs, want 23", n)
	}
}

func TestBenchRetryDueWithoutSession(t *testing.T) {
	base := filepath.Join(t.TempDir(), strings.Repeat("d", 40))
	code := 0
	if err := paths.WriteJSON(base+".marker", benchMarker{Status: "done", Exit: &code, Retry: "wanted"}); err != nil {
		t.Fatal(err)
	}
	if !benchRetryDue(base) {
		t.Fatal("a run with no session to notify never retries")
	}
}

func TestBenchQueuedOldestFirst(t *testing.T) {
	state := t.TempDir()
	now := time.Now()
	for i, sha := range []string{strings.Repeat("b", 40), strings.Repeat("a", 40)} {
		file := filepath.Join(state, sha+".job.json")
		writeBenchFixture(t, file, "{}")
		at := now.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if got := benchQueued(state); len(got) != 2 || filepath.Base(got[0]) != strings.Repeat("b", 40) {
		t.Fatalf("queue order = %v, want publication order", got)
	}
}

func TestBenchCoverageCounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, base, _, benched, merge := benchLanding(t)
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := paths.WriteJSON(filepath.Join(state, merge+".marker"), benchMarker{Status: "done", Exit: &code}); err != nil {
		t.Fatal(err)
	}
	writeBenchFixture(t, filepath.Join(state, merge+".json"), strings.ReplaceAll(cannedBench, "-3.00%", "+3.00%"))
	writeBenchFixture(t, filepath.Join(state, benched+".job.json"), "{}")
	failed := 1
	if err := paths.WriteJSON(filepath.Join(state, base+".marker"), benchMarker{Status: "failed", Exit: &failed}); err != nil {
		t.Fatal(err)
	}
	got, ok := BenchCoverage(context.Background(), dir, 10)
	want := "last 4 commits: 1 benched, 1 failed, 1 queued, 0 skipped, 1 without benchmarks; latest regression: " +
		"bench abc1234 vs def5678 internal/memory: PromptRecall/bank allocs -12.00% (p=0.000); ns +3.00% (p=0.010)"
	if got != want || ok {
		t.Fatalf("coverage = %q ok=%v\nwant %q, not ok", got, ok, want)
	}
}
