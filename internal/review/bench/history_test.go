package bench

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoryRecordsEpochChangesAndCLIReports(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(benchRoot(), "p")
	at := time.Now().UTC()
	s := Score{Name: "BenchmarkA-8", Epoch: strings.Repeat("a", 64), Source: "bench_test.go", Samples: 2,
		Metrics: map[string]Spread{"ns/op": {Median: 100}}}
	res := PkgResult{Pkg: "p", Benchmarks: []Score{s}, Compared: true}
	if _, err := recordRun(dir, Report{}, res, at); err != nil {
		t.Fatal(err)
	}
	l := loopOpts{pkg: "p", what: "profile checked", verdict: "kept", benchmark: s.Name, epoch: s.Epoch,
		productionPath: "on", subject: "present", sha: "abc"}
	if err := recordRow(l); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := RunBench([]string{"--verdicts", "--pkg", "p", "--format", "json"}, &out, &stderr); code != 0 {
		t.Fatalf("code=%d: %s", code, &stderr)
	}
	if !strings.Contains(out.String(), `"verdict":"working"`) {
		t.Fatalf("output=%s", &out)
	}
	res.Benchmarks[0].Epoch = strings.Repeat("b", 64)
	res.Benchmarks[0].Name = "BenchmarkRenamed-8"
	if _, err := recordRun(dir, Report{}, res, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	history, err := readHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil || len(history) != 3 || history[0].Schema != historySchema {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	checkRecordedEpochChange(t, s.Epoch, res.Benchmarks[0])
}

func checkRecordedEpochChange(t *testing.T, oldEpoch string, score Score) {
	t.Helper()
	rows, err := readJSONL[scorecardRow](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if err != nil || len(rows) != 2 || rows[1].Verdict != "bench-change" || rows[1].OldEpoch != oldEpoch ||
		rows[1].Epoch != score.Epoch || rows[1].Benchmark != score.Name {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestHistoryRejectsCorruptOrFutureSchema(t *testing.T) {
	file := filepath.Join(t.TempDir(), "history.jsonl")
	for _, text := range []string{"{bad\n", "{\"schema\":2}\n", "{\"schema\":1}\n"} {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHistory(file); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestEvidenceRequiresEpochAndDistinctBenchChange(t *testing.T) {
	l := loopOpts{pkg: "p", what: "change", verdict: "bench-change", benchmark: "BenchmarkA", epoch: strings.Repeat("a", 64)}
	if validateEvidence(l) == nil {
		t.Fatal("missing old epoch accepted")
	}
	l.oldEpoch = l.epoch
	if validateEvidence(l) == nil {
		t.Fatal("identical epochs accepted")
	}
	l.oldEpoch = strings.Repeat("b", 64)
	if err := validateEvidence(l); err != nil {
		t.Fatal(err)
	}
	l.productionPath = "maybe"
	if validateEvidence(l) == nil {
		t.Fatal("unknown path evidence accepted")
	}
	if validateEvidence(loopOpts{pkg: ".."}) == nil {
		t.Fatal("package escapes state directory")
	}
}

func TestRootPackageHistoryIsReported(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	l := loopOpts{pkg: ".", what: "subject removed", verdict: "reverted", benchmark: "BenchmarkA",
		epoch: strings.Repeat("a", 64), subject: "gone", sha: "abc"}
	if err := recordRow(l); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runVerdicts(loopOpts{pkg: "."}, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"verdict":"dead"`) {
		t.Fatalf("root package was not reported: %s", &out)
	}
}

func TestRecordRunConcurrentRecordsOneEpochChange(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := filepath.Join(benchRoot(), "p")
	s := Score{Name: "BenchmarkA-8", Epoch: strings.Repeat("a", 64), Source: "bench_test.go"}
	if _, err := recordRun(dir, Report{}, PkgResult{Pkg: "p", Benchmarks: []Score{s}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s.Epoch = strings.Repeat("b", 64)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := recordRun(dir, Report{}, PkgResult{Pkg: "p", Benchmarks: []Score{s}}, time.Now().UTC())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := readJSONL[scorecardRow](filepath.Join(benchRoot(), "scorecard.jsonl"))
	if err != nil || len(rows) != 1 {
		t.Fatalf("bench-change rows=%d err=%v, want 1", len(rows), err)
	}
}

func TestRecordRunStampsRepo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, top := filepath.Join(benchRoot(), "p"), t.TempDir()
	s := Score{Name: "BenchmarkA-8", Epoch: strings.Repeat("a", 64), Samples: 2}
	res := PkgResult{Pkg: "p", Benchmarks: []Score{s}, top: top}
	if _, err := recordRun(dir, Report{}, res, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, err := readHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil || len(rows) != 1 || rows[0].Repo != repoIdentity(top) || rows[0].Repo == "" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

// The worker measures in a temporary worktree; --repo makes the row name the repository the commit landed in.
func TestRecordRunStampsRepoFlag(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, worktree := t.TempDir(), t.TempDir()
	o, code, done := parseArgs([]string{"--repo", repo}, io.Discard, io.Discard)
	if done || code != 0 || checkRepo(o.repo) != nil || checkRepo("relative") == nil {
		t.Fatalf("parse --repo: %+v, exit %d", o, code)
	}
	dir := filepath.Join(benchRoot(), "p")
	s := Score{Name: "BenchmarkA-8", Epoch: strings.Repeat("a", 64), Samples: 2}
	res := PkgResult{Pkg: "p", Benchmarks: []Score{s}, top: worktree, repo: o.repo}
	if _, err := recordRun(dir, Report{}, res, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rows, err := readHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil || len(rows) != 1 || rows[0].Repo != repoIdentity(repo) {
		t.Fatalf("rows=%+v err=%v; want repo %s", rows, err, repoIdentity(repo))
	}
}

func TestBenchmarkEpochsFollowFileContents(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bench_test.go")
	source := "package p\nfunc BenchmarkA() {}\nfunc BenchmarkB() {}\n"
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(dir, "_ignored_test.go")
	if err := os.WriteFile(ignored, []byte("package p\nfunc BenchmarkIgnored() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := benchmarkEpochs(dir, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || !validEpoch(before["BenchmarkA"].Hash) || before["BenchmarkA"] != before["BenchmarkB"] {
		t.Fatalf("epochs = %+v", before)
	}
	if err := os.WriteFile(file, []byte(source+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := benchmarkEpochs(dir, ".")
	if err != nil {
		t.Fatal(err)
	}
	checkBenchmarkEpochComparisons(t, before, after)
	checkCRLFKeepsEpoch(t, dir, file, source+"\n", after)
}

func checkCRLFKeepsEpoch(t *testing.T, dir, file, source string, want map[string]benchmarkEpoch) {
	t.Helper()
	if err := os.WriteFile(file, []byte(strings.ReplaceAll(source, "\n", "\r\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	crlf, err := benchmarkEpochs(dir, ".")
	if err != nil || !sameEpochs(epochHeader(crlf), epochHeader(want)) {
		t.Fatalf("CRLF checkout changed the epoch: %+v err=%v", crlf, err)
	}
}

func checkBenchmarkEpochComparisons(t *testing.T, before, after map[string]benchmarkEpoch) {
	t.Helper()
	if sameEpochs(epochHeader(before), epochHeader(after)) || !sameEpochs(epochHeader(before), epochHeader(before)) {
		t.Fatal("file rewrite did not isolate epochs")
	}
	if benchmarkFunc("BenchmarkA/case-18") != "BenchmarkA" || benchmarkFunc("BenchmarkA-18") != "BenchmarkA" {
		t.Fatal("runtime benchmark name did not resolve to its source function")
	}
	if sameEpochs("legacy", epochHeader(after)) || sameEpochs(epochPrefix+"{bad", epochHeader(after)) {
		t.Fatal("unknown or corrupt epoch was comparable")
	}
}

func TestEpochChangeRebaselinesOnlyCleanWithoutComparing(t *testing.T) {
	dir := t.TempDir()
	before := epochHeader(map[string]benchmarkEpoch{"BenchmarkA": {File: "bench_test.go", Hash: strings.Repeat("a", 64)}})
	after := epochHeader(map[string]benchmarkEpoch{"BenchmarkA": {File: "bench_test.go", Hash: strings.Repeat("b", 64)}})
	before += "BenchmarkA-8 1 100 ns/op\n"
	after += "BenchmarkA-8 1 1 ns/op\n"
	b, a := filepath.Join(dir, "rolling.txt"), filepath.Join(dir, "after.txt")
	if err := os.WriteFile(b, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := decideFiles(b, a)
	if err != nil || !d.NoComparison || d.Keep {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
	checkDirtyKeepsBaseline(t, dir, a, before, after)
	res := PkgResult{File: a}
	if err := compareRolling(&res, dir, after, false, true); err != nil {
		t.Fatal(err)
	}
	checkEpochBaseline(t, res, b, after)
}

func checkDirtyKeepsBaseline(t *testing.T, dir, file, before, after string) {
	t.Helper()
	res := PkgResult{File: file}
	if err := compareRolling(&res, dir, after, false, false); err != nil || res.Baseline || !res.Stale {
		t.Fatalf("dirty tree replaced the baseline: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "rolling.txt")); string(got) != before {
		t.Fatalf("dirty tree rewrote rolling.txt: %q", got)
	}
}

func checkEpochBaseline(t *testing.T, res PkgResult, file, after string) {
	t.Helper()
	if res.Compared || !res.Baseline || len(res.Missing) != 1 {
		t.Fatalf("result=%+v", res)
	}
	got, err := os.ReadFile(file)
	if err != nil || string(got) != after {
		t.Fatalf("baseline=%q err=%v", got, err)
	}
}

func TestBenchmarkVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := observation{At: now, Pkg: "p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64),
		Compared: true, ProductionPath: "on", Subject: "present",
		Score: &Score{Samples: 2, CostSec: 2, Metrics: map[string]Spread{"ns/op": {Median: 100, CV: 1}}}}
	cases := []struct {
		name, want string
		change     func(*observation)
	}{
		{"stable compared", "working", func(*observation) {}},
		{"gone", "dead", func(r *observation) { r.Subject = "gone" }},
		{"off path", "off-path", func(r *observation) { r.ProductionPath = "off" }},
		{"unverified", "unverified", func(r *observation) { r.ProductionPath = "" }},
		{"never compared", "idle", func(r *observation) { r.ProductionPath, r.Compared = "", false }},
		{"no samples", "idle", func(r *observation) { r.Score = nil }},
		{"within run noise", "noisy", func(r *observation) { r.Score.Metrics = map[string]Spread{"ns/op": {CV: 6}} }},
		{"loaded", "noisy", func(r *observation) { r.Score.Flags = []string{"load"} }},
		{"expensive sample", "costly", func(r *observation) { r.Score.CostSec = 5 }},
		{"never decisive", "idle", func(r *observation) { r.At = now.Add(-idleAge) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, score := base, *base.Score
			row.Score = &score
			tc.change(&row)
			got := evaluateHistory([]observation{row}, now)
			if len(got) != 1 || got[0].Verdict != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestVerdictsResetHistoryAtEpochAndIgnoreOtherHosts(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	old := observation{At: now.Add(-time.Hour), Pkg: "p", Benchmark: "BenchmarkA-8", Epoch: strings.Repeat("a", 64),
		ProductionPath: "on", Compared: true,
		Score: &Score{Samples: 2, Metrics: map[string]Spread{"ns/op": {Median: 100}}}}
	newer := old
	newer.At, newer.ProductionPath = now, ""
	newer.Score = &Score{Samples: 2, Metrics: map[string]Spread{"ns/op": {Median: 200}}}
	if got := evaluateHistory([]observation{newer, old}, now); got[0].Verdict != "noisy" {
		t.Fatalf("cross-run CV: %+v", got)
	}
	newer.Env.Machine = "other host"
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "working" {
		t.Fatalf("host isolation: %+v", got)
	}
	newer.Epoch = strings.Repeat("b", 64)
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "unverified" {
		t.Fatalf("epoch leaked path evidence: %+v", got)
	}
	newer.ProductionPath, newer.Decisive = "on", true
	if got := evaluateHistory([]observation{old, newer}, now); got[0].Verdict != "working" {
		t.Fatalf("new baseline: %+v", got)
	}
}
