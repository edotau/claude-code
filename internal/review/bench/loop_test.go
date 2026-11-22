package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRankWeakFirstThenScoreAndSkipSettled(t *testing.T) {
	items := []Item{
		{Kind: "fix", Pkg: "p", What: "small", CallsPerDay: 10, EstSavingMS: 1},
		{Kind: "fix", Pkg: "p", What: "big", CallsPerDay: 100, EstSavingMS: 1},
		{Kind: "fix", Pkg: "p", What: "done", CallsPerDay: 99, EstSavingMS: 1},
		{Kind: "fix", Pkg: "q", What: "redo", CallsPerDay: 5, EstSavingMS: 1},
		{Kind: "weak", Pkg: "p", What: "BenchmarkX is noisy"},
	}
	rows := []Row{
		{Pkg: "p", What: "done", Verdict: "kept", At: time.Now()},
		{Pkg: "q", What: "redo", Verdict: "reverted", At: time.Now()},
	}
	got := rank(items, rows, func(pkg string, _ time.Time) bool { return pkg == "q" })
	var names []string
	for _, g := range got {
		names = append(names, g.What)
	}
	if want := "BenchmarkX is noisy,big,small,redo"; strings.Join(names, ",") != want {
		t.Errorf("order = %v, want %s", names, want)
	}
}

func TestDecide(t *testing.T) {
	d := func(c string) Delta { return Delta{Change: c, Significant: c != "~"} }
	quiet := []Score{{Name: "BenchmarkA-8"}}
	noisy := []Score{{Name: "BenchmarkA-8", Flags: []string{"noisy"}}}
	faster := map[string]map[string]Delta{"A-8": {"sec/op": d("-20.00%"), "B/op": d("~")}}
	if got := decide(faster, quiet); !got.Keep || len(got.Rows) != 1 {
		t.Errorf("faster on quiet run: %+v", got)
	}
	if got := decide(faster, noisy); got.Keep {
		t.Errorf("ns/op win on a noisy run kept: %+v", got)
	}
	allocs := map[string]map[string]Delta{"A-8": {"sec/op": d("-20.00%"), "allocs/op": d("-40.00%")}}
	if got := decide(allocs, noisy); !got.Keep {
		t.Errorf("noisy but allocs improved must keep: %+v", got)
	}
	worse := map[string]map[string]Delta{"A-8": {"sec/op": d("-20.00%"), "B/op": d("+9.00%")}}
	if got := decide(worse, quiet); got.Keep || !strings.HasPrefix(got.Rows[0], "regressed") {
		t.Errorf("a regression must revert: %+v", got)
	}
	if decide(map[string]map[string]Delta{"A-8": {"sec/op": d("~")}}, quiet).Keep {
		t.Error("no move kept")
	}
}

func TestRecordAppendsAndAdvancesRolling(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	after := filepath.Join(t.TempDir(), "after.txt")
	if err := os.WriteFile(after, []byte("BenchmarkA-8 1 1 ns/op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loopOpts{pkg: "internal/x/y", what: "w", verdict: "kept", after: after, agent: "claude", sha: "abc", attempts: 1}
	if err := recordRow(l); err != nil {
		t.Fatal(err)
	}
	rows := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if len(rows) != 1 || rows[0].Pkg != "internal/x/y" || rows[0].Verdict != "kept" || rows[0].Sha != "abc" {
		t.Fatalf("rows = %+v", rows)
	}
	if b, err := os.ReadFile(filepath.Join(benchRoot(), "x-y", "rolling.txt")); err != nil || !strings.Contains(string(b), "BenchmarkA") {
		t.Errorf("rolling.txt = %q, %v", b, err)
	}
	if recordRow(loopOpts{pkg: "p", what: "w", verdict: "kept"}) == nil {
		t.Error("kept without --after accepted")
	}
}

func TestSeedFileWritesFiveOnce(t *testing.T) {
	f := filepath.Join(t.TempDir(), "candidates.jsonl")
	for range 2 {
		if err := seedFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if got := readJSONL[Item](f); len(got) != 5 || got[0].Source != "baseline" {
		t.Errorf("seed = %+v", got)
	}
}
