package bench

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestRankWeakAbovePackageFixThenScoreAndSkipSettled(t *testing.T) {
	items := []Item{
		{Kind: "fix", Pkg: "p", What: "small", CallsPerDay: 10, EstSavingMS: 1},
		{Kind: "fix", Pkg: "p", What: "big", CallsPerDay: 100, EstSavingMS: 1},
		{Kind: "fix", Pkg: "p", What: "done", CallsPerDay: 99, EstSavingMS: 1},
		{Kind: "fix", Pkg: "q", What: "redo", CallsPerDay: 5, EstSavingMS: 1},
		{Kind: "weak", Pkg: "p", What: "BenchmarkX is noisy"},
		{Kind: "weak", Pkg: "p", What: "BenchmarkX is slow"},
		{Kind: "weak", Pkg: "z", What: "BenchmarkZ is noisy"},
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
	if want := "BenchmarkX is noisy, slow;big;small;redo;BenchmarkZ is noisy"; strings.Join(names, ";") != want {
		t.Errorf("order = %v, want %s", names, want)
	}
}

func decideDelta(t *testing.T, c string) Delta {
	t.Helper()
	pct := 0.0
	if c != "~" {
		var err error
		pct, err = strconv.ParseFloat(strings.TrimSuffix(c, "%"), 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	return Delta{Old: "100", New: strconv.FormatFloat(100+pct, 'f', -1, 64), Change: c, Significant: c != "~"}
}

func TestDecide(t *testing.T) {
	d := func(c string) Delta { return decideDelta(t, c) }
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

func TestDecideNoisyTimingIsInconclusive(t *testing.T) {
	d := func(c string) Delta { return decideDelta(t, c) }
	quiet := []Score{{Name: "BenchmarkA-8"}}
	noisy := []Score{{Name: "BenchmarkA-8", Flags: []string{"noisy"}}}
	one := func(u, c string) map[string]map[string]Delta { return map[string]map[string]Delta{"A-8": {u: d(c)}} }
	slower, faster := one("sec/op", "+20.00%"), one("sec/op", "-20.00%")
	hidden := map[string]map[string]Delta{"A-8": {"sec/op": d("+20.00%"), "allocs/op": d("-40.00%")}}
	both := map[string]map[string]Delta{"A-8": {"sec/op": d("+20.00%"), "allocs/op": d("+40.00%")}}
	for _, c := range []struct {
		name          string
		deltas        map[string]map[string]Delta
		before, after []Score
		noComparison  bool
	}{
		{"quiet timing regression reverts", slower, quiet, quiet, false},
		{"noisy timing regression is inconclusive", slower, quiet, noisy, true},
		{"timing win only on a noisy run", faster, noisy, quiet, true},
		{"allocs win over a hidden timing regression", hidden, noisy, quiet, true},
		{"counted allocs regression still reverts", both, noisy, quiet, false},
		{"noisy run with nothing significant", one("sec/op", "~"), quiet, noisy, true},
		{"quiet run with nothing significant reverts", one("sec/op", "~"), quiet, quiet, false},
	} {
		if got := decide(c.deltas, c.before, c.after); got.Keep || got.NoComparison != c.noComparison {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	if got := decide(slower, quiet, quiet); !strings.HasPrefix(got.Rows[0], "regressed") {
		t.Errorf("a quiet timing regression row: %+v", got)
	}
	if got := decide(faster, quiet, quiet); !slices.Equal(got.Faster, []string{"A-8"}) {
		t.Errorf("Faster = %v, want [A-8]", got.Faster)
	}
}

func TestDecideMinRegressionAndTimeWeighted(t *testing.T) {
	d := func(c string) Delta { return decideDelta(t, c) }
	values := func(old, next, change string) Delta {
		return Delta{Old: old, New: next, Change: change, Significant: true}
	}
	quiet := []Score{{Name: "BenchmarkA-8"}}
	cases := []struct {
		name  string
		units map[string]Delta
		keep  bool
		rule  string
	}{
		{"zero baseline allocations", map[string]Delta{"B/op": values("0", "16", "?"),
			"allocs/op": values("0", "1", "?")}, false, "reverts"},
		{"zero baseline blocks speedup", map[string]Delta{"sec/op": d("-20%"), "B/op": values("0", "16", "?")}, false, "reverts"},
		{"positive to zero", map[string]Delta{"B/op": values("16", "0", "?")}, true, "significant improvement"},
		{"zero to zero", map[string]Delta{"B/op": values("0", "0", "?")}, false, "no significant improvement"},
		{"numeric regression", map[string]Delta{"B/op": values("16", "32", "?")}, false, "reverts"},
		{"numeric improvement", map[string]Delta{"B/op": values("32", "16", "?")}, true, "significant improvement"},
		{"unparsed values", map[string]Delta{"B/op": values("bad", "worse", "?")}, false, "no significant improvement"},
		{"unparsed new value", map[string]Delta{"B/op": values("16", "bad", "-10%")}, false, "no significant improvement"},
		{"nonfinite old value", map[string]Delta{"B/op": values("NaN", "16", "?")}, false, "no significant improvement"},
		{"nonfinite new value", map[string]Delta{"B/op": values("16", "+Inf", "?")}, false, "no significant improvement"},
		{"negative value", map[string]Delta{"B/op": values("16", "-1", "?")}, false, "no significant improvement"},
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

func TestRecordAppendsAndDefersRolling(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	_, after := pendingFixture(t, "internal/x/y")
	rolling := filepath.Join(benchRoot(), "x-y", "rolling.txt")
	if err := paths.AtomicWrite(rolling, []byte("old baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := loopOpts{pkg: "internal/x/y", what: "w", verdict: "kept", after: after,
		lane: "bench-xy", agent: "claude", sha: "abc", attempts: 1}
	if err := recordRow(l); err != nil {
		t.Fatal(err)
	}
	rows, err := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	want := Row{At: rows[0].At, Pkg: l.pkg, What: l.what, Verdict: l.verdict, Agent: l.agent, Sha: l.sha, Attempts: 1}
	if rows[0] != want {
		t.Errorf("row = %+v, want %+v", rows[0], want)
	}
	if b, err := os.ReadFile(rolling); err != nil || string(b) != "old baseline" {
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

func TestQueueKindFilter(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var buf strings.Builder
	if err := runQueue(loopOpts{kind: "weak", top: 5}, false, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "queue empty" {
		t.Errorf("--kind weak over fix-only seeds:\n%s", buf.String())
	}
	buf.Reset()
	if err := runQueue(loopOpts{kind: "fix", top: 1}, false, &buf); err != nil || !strings.HasPrefix(buf.String(), "1. [fix]") {
		t.Errorf("--kind fix: %q %v", buf.String(), err)
	}
}

func TestFromReportQueuesOnlyCleanRegressions(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Score)
		want int
	}{
		{"clean", regressed, 1},
		{"load held", func(s *Score) { regressed(s); s.Flags = []string{"load"} }, 0},
		{"empty", func(s *Score) { regressed(s); s.Samples = 1 }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := *autoRow(time.Now(), tc.mod).Score
			s.Name = "BenchmarkA-8"
			got := fromReport(Report{Packages: []PkgResult{{Pkg: "internal/p", Benchmarks: []Score{s}}}}, "r.json")
			if len(got) != tc.want {
				t.Fatalf("queued %+v, want %d", got, tc.want)
			}
		})
	}
}

func TestQueueMergesHistoryOncePerBenchmark(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	rows := []observation{autoRow(now.Add(-2*time.Hour), noisyRun), autoRow(now.Add(-time.Hour), noisyRun),
		{At: now, Pkg: "internal/p", Benchmark: "BenchmarkB-8", Epoch: strings.Repeat("b", 64), Score: autoRow(now, regressed).Score}}
	if err := appendHistory(filepath.Join(benchRoot(), "p"), rows); err != nil {
		t.Fatal(err)
	}
	writeBenchFile(t, "candidates.jsonl", `{"pkg":"internal/p","what":"BenchmarkB regressed +3.00%","calls_per_day":1}`)
	var buf strings.Builder
	if err := runQueue(loopOpts{kind: "benchmark"}, false, &buf); err != nil || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("--kind benchmark: %q %v", buf.String(), err)
	}
	buf.Reset()
	if err := runQueue(loopOpts{kind: "fix"}, false, &buf); err != nil || strings.Count(buf.String(), "BenchmarkB") != 1 ||
		!strings.Contains(buf.String(), "+10.00%") {
		t.Fatalf("--kind fix keeps one BenchmarkB item, the larger saving: %q %v", buf.String(), err)
	}
}

func TestRecordRowScorecardFailureLeavesNoPending(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, after := pendingFixture(t, "internal/p")
	if err := os.MkdirAll(filepath.Join(benchRoot(), "scorecard.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := loopOpts{pkg: "internal/p", what: "speed", verdict: "kept", after: after, sha: "abc", lane: "bench-p"}
	if err := recordRow(l); err == nil {
		t.Fatal("recordRow succeeded with an unwritable scorecard")
	}
	if _, err := os.Stat(pendingFile()); !os.IsNotExist(err) {
		t.Fatalf("failed scorecard write left pending state: %v", err)
	}
}
