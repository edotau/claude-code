package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const abEpochs = `# bench-epochs: {"BenchmarkA":{"file":"a_test.go","hash":"` +
	"0000000000000000000000000000000000000000000000000000000000000000" + `"}}` + "\n"

func abRun(sha string, dirty bool, ns int) string {
	return fmt.Sprintf("%s# commit: %s\n# dirty: %t\n# cpus: 8\n# load-before: 0.50\n# load-after: 0.60\n"+
		"goos: darwin\nBenchmarkA-8\t1000000\t%d ns/op\nPASS\n", abEpochs, sha, dirty, ns)
}

// stubSeam records the order of runs, and the most that ran at once.
func stubSeam(order *[]string, live, peak *atomic.Int32, dirty map[string]bool) abSeam {
	return abSeam{
		commit: func(dir string) (string, error) { return filepath.Base(dir), nil },
		dirty:  func(dir string) (bool, error) { return dirty[filepath.Base(dir)], nil },
		bench: func(_ context.Context, dir string, _ ABOptions, out string) error {
			n := live.Add(1)
			defer live.Add(-1)
			peak.Store(max(peak.Load(), n))
			*order = append(*order, filepath.Base(dir))
			return os.WriteFile(out, []byte(abRun(filepath.Base(dir), false, 1000+len(*order))), 0o644)
		},
		decide: func(before, after string) (Decision, error) {
			return Decision{Keep: true, Rule: before + "|" + after}, nil
		},
	}
}

func abDirs(t *testing.T) (string, string) {
	root := t.TempDir()
	a, b := filepath.Join(root, "aaa111"), filepath.Join(root, "bbb222")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func TestInterleaveAlternatesMergesAndNamesCommits(t *testing.T) {
	a, b := abDirs(t)
	var order []string
	var live, peak atomic.Int32
	var log strings.Builder
	o := ABOptions{A: a, B: b, Pkg: "internal/x", Rounds: 3, Count: 2, Work: t.TempDir()}
	res, err := interleave(context.Background(), o, stubSeam(&order, &live, &peak, nil), &log)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(res.Before)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		ok  bool
		msg string
	}{
		{strings.Join(order, ",") == "aaa111,bbb222,bbb222,aaa111,aaa111,bbb222", fmt.Sprintf("order = %v, want ABBA", order)},
		{peak.Load() == 1, fmt.Sprintf("peak concurrent benchmark runs = %d, want 1", peak.Load())},
		{strings.Contains(log.String(), "A aaa111"), "log lacks A commit:\n" + log.String()},
		{strings.Contains(log.String(), "B bbb222"), "log lacks B commit:\n" + log.String()},
		{res.Decision.Keep, fmt.Sprintf("result = %+v", res)},
		{res.CommitA == "aaa111", fmt.Sprintf("result = %+v", res)},
		{res.CommitB == "bbb222", fmt.Sprintf("result = %+v", res)},
		{strings.Count(string(before), "BenchmarkA-8") == 3, fmt.Sprintf("merged before holds %d samples, want 3:\n%s", strings.Count(string(before), "BenchmarkA-8"), before)},
		{strings.Count(string(before), epochPrefix) == 1, fmt.Sprintf("merged before headers wrong:\n%s", before)},
		{!loadNoisy(string(before)), fmt.Sprintf("merged before headers wrong:\n%s", before)},
	}
	for _, check := range checks {
		if !check.ok {
			t.Error(check.msg)
		}
	}
}

func TestInterleaveRefusals(t *testing.T) {
	a, b := abDirs(t)
	var order []string
	var live, peak atomic.Int32
	o := ABOptions{A: a, B: b, Pkg: "internal/x", Rounds: 1, Count: 1, Work: t.TempDir()}
	if _, err := interleave(context.Background(), o, stubSeam(&order, &live, &peak, map[string]bool{"bbb222": true}), io.Discard); err == nil ||
		!strings.Contains(err.Error(), "dirty") || len(order) != 0 {
		t.Errorf("dirty B: err = %v, runs = %v", err, order)
	}
	same := stubSeam(&order, &live, &peak, nil)
	same.commit = func(string) (string, error) { return "abc", nil }
	if _, err := interleave(context.Background(), o, same, io.Discard); err == nil || len(order) != 0 {
		t.Errorf("same commit: err = %v, runs = %v", err, order)
	}
	if _, err := interleave(context.Background(), ABOptions{A: a, B: a, Pkg: "p", Rounds: 1, Count: 1}, same, io.Discard); err == nil {
		t.Error("same worktree accepted")
	}
}

func TestInterleaveRejectsWrongCommitRound(t *testing.T) {
	a, b := abDirs(t)
	var order []string
	var live, peak atomic.Int32
	s := stubSeam(&order, &live, &peak, nil)
	s.bench = func(_ context.Context, _ string, _ ABOptions, out string) error {
		return os.WriteFile(out, []byte(abRun("zzz999", false, 1000)), 0o644)
	}
	o := ABOptions{A: a, B: b, Pkg: "internal/x", Rounds: 1, Count: 1, Work: t.TempDir()}
	if _, err := interleave(context.Background(), o, s, io.Discard); err == nil || !strings.Contains(err.Error(), "not a run of commit") {
		t.Errorf("err = %v", err)
	}
}

func TestInterleaveAbortsOnNoisyRound(t *testing.T) {
	a, b := abDirs(t)
	var order []string
	var live, peak atomic.Int32
	s := stubSeam(&order, &live, &peak, nil)
	s.bench = func(_ context.Context, dir string, _ ABOptions, out string) error {
		order = append(order, filepath.Base(dir))
		const nsPerOp = 1000
		run := abRun(filepath.Base(dir), false, nsPerOp)
		if len(order) == 2 { // load rises above cpus/2 during the first B run
			run = strings.Replace(run, "# load-after: 0.60", "# load-after: 7.00", 1)
		}
		return os.WriteFile(out, []byte(run), 0o644)
	}
	o := ABOptions{A: a, B: b, Pkg: "internal/x", Rounds: 3, Count: 1, Work: t.TempDir()}
	if _, err := interleave(context.Background(), o, s, io.Discard); err == nil || !strings.Contains(err.Error(), "load") || len(order) != 2 {
		t.Errorf("err = %v, runs = %v; want a load abort after 2 runs", err, order)
	}
}
