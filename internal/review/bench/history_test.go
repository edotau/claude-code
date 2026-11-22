package bench

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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
	rows, err := readJSONL[Row](filepath.Join(benchRoot(), "scorecard.jsonl"))
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
