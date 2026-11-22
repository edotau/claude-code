package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
		{Pkg: "q", What: "redo", Verdict: "reverted", At: time.Now().Add(-8 * 24 * time.Hour)},
	}
	got := rank(items, rows, time.Now())
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
	if got := decide(faster, quiet, quiet); !got.Keep || len(got.Rows) != 1 {
		t.Errorf("faster on quiet run: %+v", got)
	}
	if got := decide(faster, quiet, noisy); got.Keep {
		t.Errorf("ns/op win on a noisy run kept: %+v", got)
	}
	allocs := map[string]map[string]Delta{"A-8": {"sec/op": d("-20.00%"), "allocs/op": d("-40.00%")}}
	if got := decide(faster, noisy, quiet); got.Keep {
		t.Errorf("ns/op win with a noisy before run kept: %+v", got)
	}
	if got := decide(allocs, noisy, noisy); !got.Keep {
		t.Errorf("noisy but allocs improved must keep: %+v", got)
	}
	worse := map[string]map[string]Delta{"A-8": {"sec/op": d("-20.00%"), "B/op": d("+29.00%")}}
	if got := decide(worse, quiet, quiet); got.Keep || !strings.HasPrefix(got.Rows[0], "regressed") {
		t.Errorf("a regression must revert: %+v", got)
	}
	if decide(map[string]map[string]Delta{"A-8": {"sec/op": d("~")}}, quiet, quiet).Keep {
		t.Error("no move kept")
	}
}

func TestDecideMinRegressionAndTimeWeighted(t *testing.T) {
	d := func(c string) Delta { return Delta{Change: c, Significant: true} }
	quiet := []Score{{Name: "BenchmarkA-8"}}
	cases := []struct {
		name  string
		units map[string]Delta
		keep  bool
		rule  string
	}{
		{"minor memory", map[string]Delta{"sec/op": d("-15.76%"), "allocs/op": d("+1.47%")}, true, "minor"},
		{"minor time", map[string]Delta{"sec/op": d("+1.50%"), "B/op": d("-10.00%")}, true, "minor"},
		{"speedup outweighs", map[string]Delta{"sec/op": d("-15.00%"), "B/op": d("+9.00%")}, true, "time-weighted"},
		{"memory exceeds speedup", map[string]Delta{"sec/op": d("-5.00%"), "B/op": d("+9.00%")}, false, "reverts"},
		{"slower blocks weighting", map[string]Delta{"sec/op": d("+3.00%"), "allocs/op": d("-50.00%"),
			"B/op": d("+2.50%")}, false, "reverts"},
	}
	for _, c := range cases {
		got := decide(map[string]map[string]Delta{"A-8": c.units}, quiet, quiet)
		if got.Keep != c.keep || !strings.Contains(got.Rule, c.rule) {
			t.Errorf("%s: keep=%v rule=%q, want keep=%v rule~%q", c.name, got.Keep, got.Rule, c.keep, c.rule)
		}
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
	rows, err := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if err != nil || len(rows) != 1 || rows[0].Pkg != "internal/x/y" || rows[0].Verdict != "kept" || rows[0].Sha != "abc" {
		t.Fatalf("rows = %+v", rows)
	}
	if b, err := os.ReadFile(filepath.Join(benchRoot(), "x-y", "rolling.txt")); err != nil || !strings.Contains(string(b), "BenchmarkA") {
		t.Errorf("rolling.txt = %q, %v", b, err)
	}
}

func TestRecordKeptWithoutAfterKeepsBaseline(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rolling := filepath.Join(benchRoot(), "p", "rolling.txt")
	if err := os.MkdirAll(filepath.Dir(rolling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolling, []byte("BenchmarkOld-8 1 5 ns/op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordRow(loopOpts{pkg: "internal/p", what: "w", verdict: "kept", sha: "abc"}); err != nil {
		t.Errorf("kept without --after (no benchmark) refused: %v", err)
	}
	if b, err := os.ReadFile(rolling); err != nil || string(b) != "BenchmarkOld-8 1 5 ns/op\n" {
		t.Errorf("rolling.txt changed: %q, %v", b, err)
	}
}

func TestDecideFilesNoCommonBenchmark(t *testing.T) {
	dir := t.TempDir()
	before, after := filepath.Join(dir, "b.txt"), filepath.Join(dir, "a.txt")
	_ = os.WriteFile(before, []byte("BenchmarkOld-8 1 5 ns/op\n"), 0o644)
	_ = os.WriteFile(after, []byte("BenchmarkNew-8 1 5 ns/op\n"), 0o644)
	d, err := decideFiles(before, after)
	if err != nil || !d.NoComparison || d.Keep || len(d.Rows) != 2 {
		t.Errorf("renamed benchmark: %+v %v, want NoComparison naming both sides", d, err)
	}
}

func TestRecordFillsFromDispatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := `{"agent":"claude","model":"m1","duration_ms":4000,"usage":{"input_tokens":7,"output_tokens":3,"cache_read_tokens":5}}`
	if os.MkdirAll(paths.DispatchDir(), 0o755) != nil || os.WriteFile(filepath.Join(paths.DispatchDir(), "id1.json"), []byte(rec), 0o644) != nil {
		t.Fatal("seed dispatch record")
	}
	if err := recordRow(loopOpts{pkg: "internal/p", what: "w", verdict: "reverted", sha: "abc", dispatch: "id1"}); err != nil {
		t.Fatal(err)
	}
	r, err := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := Row{Pkg: "internal/p", What: "w", Verdict: "reverted", Agent: "claude", Model: "m1", TokensIn: 7, TokensOut: 3,
		TokensCache: 5, DurationS: 4, Sha: "abc"}
	if len(r) != 1 || r[0].At.IsZero() {
		t.Fatalf("rows = %+v", r)
	}
	if r[0].At = (time.Time{}); r[0] != want {
		t.Errorf("row = %+v, want %+v", r[0], want)
	}
	if recordRow(loopOpts{pkg: "p", what: "w", verdict: "kept", sha: "a", dispatch: "../x"}) == nil {
		t.Error("a path as --dispatch was read")
	}
}

func TestSeedFileWritesFiveOnce(t *testing.T) {
	f := filepath.Join(t.TempDir(), "candidates.jsonl")
	for range 2 {
		if err := seedFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := readJSONL[Item](f); err != nil || len(got) != 5 || got[0].Source != "baseline" {
		t.Errorf("seed = %+v", got)
	}
}

func TestRankClosedRules(t *testing.T) {
	now := time.Now()
	items := []Item{
		{Kind: "fix", Pkg: "p", What: "Kept thing: seed text"},
		{Kind: "fix", Pkg: "p", What: "rev thing: seed"},
		{Kind: "fix", Pkg: "p", What: "other"},
	}
	rows := []Row{
		{Pkg: "p", What: "kept thing: recorded text", Verdict: "kept", At: now.Add(-30 * 24 * time.Hour)},
		{Pkg: "p", What: "rev thing: x", Verdict: "reverted", At: now.Add(-24 * time.Hour)},
	}
	got := rank(items, rows, now)
	if len(got) != 1 || got[0].What != "other" {
		t.Fatalf("got %+v, want only other", got)
	}
	got = rank(items, rows, now.Add(8*24*time.Hour))
	if len(got) != 2 {
		t.Errorf("after 7 days the reverted item must return: %+v", got)
	}
}
