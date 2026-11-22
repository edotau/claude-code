package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestEpochChangeStartsBaselineWithoutComparing(t *testing.T) {
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
	res := PkgResult{File: a}
	if err := compareRolling(&res, dir, after, false, false); err != nil {
		t.Fatal(err)
	}
	checkEpochBaseline(t, res, b, after)
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
