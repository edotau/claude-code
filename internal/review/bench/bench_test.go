package bench

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const canned = `goos: darwin
BenchmarkA-8   	  1000000	      1000 ns/op	     64 B/op	       2 allocs/op
BenchmarkA-8   	  1000000	      1300 ns/op	     64 B/op	       2 allocs/op
BenchmarkB-8   	        10	 900000000 ns/op	      42 corpus-bytes
PASS
`

const cannedCSV = `goos: darwin
,old.txt,,new.txt,,,
,sec/op,CI,sec/op,CI,vs base,P
A-8,1e-06,2%,1.3e-06,2%,+30.00%,p=0.002 n=2
B-8,1e-06,2%,1e-06,2%,~,p=1.000 n=2
geomean,1e-06,,1e-06,,+1.00%,
,B/op,CI,B/op,CI,vs base,P
A-8,64,0%,64,0%,~,p=1.000 n=2
`

func TestScoreFlagsNoiseAndCost(t *testing.T) {
	got := scoreAll(canned, false)
	if len(got) != 2 {
		t.Fatalf("benchmarks = %d, want 2", len(got))
	}
	a, b := got[0], got[1]
	if a.NoisePct != 30 || a.Metrics["ns/op"].Median != 1150 || a.Metrics["B/op"].Median != 64 || a.Metrics["allocs/op"].Max != 2 || len(a.Flags) != 1 || a.Flags[0] != "noisy" {
		t.Errorf("A = %+v", a)
	}
	checkB(t, b)
}

func checkB(t *testing.T, b Score) {
	if b.Metrics["corpus-bytes"].Median != 42 {
		t.Errorf("custom metric lost: %+v", b.Metrics)
	}
	if b.CostSec != 9 || len(b.Flags) != 1 || b.Flags[0] != "slow" {
		t.Errorf("B = %+v", b)
	}
}

func TestParseBenchstatCSVAndLoad(t *testing.T) {
	d := parseBenchstatCSV(cannedCSV)
	if !d["A-8"]["sec/op"].Significant || d["B-8"]["sec/op"].Significant || d["A-8"]["B/op"].Significant {
		t.Errorf("deltas = %+v", d)
	}
	if _, ok := d["geomean"]; ok {
		t.Error("geomean row kept")
	}
	if parseLoad("{ 2.46 2.29 2.39 }\n") != 2.46 || parseLoad("0.50 0.4 0.3 1/200 99") != 0.5 || parseLoad("") != 0 {
		t.Error("parseLoad")
	}
}

func TestSpreadCV(t *testing.T) {
	sp := spread([]float64{2, 4, 6})
	if sp.Min != 2 || sp.Median != 4 || sp.Max != 6 || sp.CV != 50 {
		t.Errorf("spread = %+v", sp)
	}
}

func TestParseMemory(t *testing.T) {
	vm := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:   64.\nPages inactive:   64.\nPages active: 9.\n"
	if got := parseVMStat(vm); got != 2 {
		t.Errorf("vm_stat MB = %d, want 2", got)
	}
	if tot, av := parseMeminfo("MemTotal: 2048 kB\nMemAvailable: 1024 kB\n"); tot != 2 || av != 1 {
		t.Errorf("meminfo = %d %d", tot, av)
	}
}

func TestMissingBenchNamesRemovedBenchmark(t *testing.T) {
	got := missingBench(canned, "BenchmarkA-8 1 1000 ns/op 64 B/op 2 allocs/op\n")
	want := "missing after: BenchmarkB-8 ns/op"
	if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), want) {
		t.Errorf("missing = %v, want it to hold %q", got, want)
	}
	if got := missingBench(canned, canned); len(got) != 0 {
		t.Errorf("identical runs missing %v", got)
	}
}

func TestScoreNoisyOnCVNotRange(t *testing.T) {
	var b strings.Builder
	for i := range 20 { // many samples widen the range, not the CV: one 15% outlier in 20
		ns := "1000"
		if i == 0 {
			ns = "1150"
		}
		fmt.Fprintf(&b, "BenchmarkC-8 3000000 %s ns/op\n", ns) // 3 s a sample
	}
	if c := scoreAll(b.String(), false)[0]; c.NoisePct != 15 || slices.Contains(c.Flags, "noisy") || !slices.Contains(c.Flags, "slow") {
		t.Errorf("C = %+v, want quiet (CV %.1f) and slow", c, c.Metrics["ns/op"].CV)
	}
}

func TestCompareRollingSeedsFirstCleanRun(t *testing.T) {
	dir := t.TempDir()
	res := PkgResult{File: dir + "/run.txt"}
	if err := compareRolling(&res, dir, "dirty", false, false); err != nil || res.Baseline {
		t.Fatalf("dirty run seeded: %+v %v", res, err)
	}
	if err := compareRolling(&res, dir, "clean", false, true); err != nil || !res.Baseline || res.Compared {
		t.Fatalf("clean first run: %+v %v", res, err)
	}
	if b, err := os.ReadFile(dir + "/rolling.txt"); err != nil || string(b) != "clean" {
		t.Errorf("rolling.txt = %q, %v", b, err)
	}
}

func TestWriteRunFileNeverOverwrites(t *testing.T) {
	dir, at := t.TempDir(), time.Now()
	rep := Report{Mode: "run"}
	file, err := writeRunFile(dir, at, rep, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeRunFile(dir, at, rep, "second"); err == nil {
		t.Error("second run in the same second overwrote the first")
	}
	if b, _ := os.ReadFile(file); string(b) != "first" {
		t.Errorf("run file = %q", b)
	}
}

func TestWriteOutCopiesRunText(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run.txt")
	if err := os.WriteFile(run, []byte("# cpus: 8\nBenchmarkX 1 5 ns/op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "sub", "before.txt")
	if err := writeOut(out, run); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "BenchmarkX") {
		t.Errorf("out = %q", b)
	}
	if err := writeOut("", run); err != nil {
		t.Errorf("empty out: %v", err)
	}
}

func TestParseArgsOutAndKindConflicts(t *testing.T) {
	for _, args := range [][]string{{"--kind", "fix"}, {"--queue", "--kind", "nope"}, {"--queue", "--out", "f"}} {
		var stderr bytes.Buffer
		if _, code, done := parseArgs(args, io.Discard, &stderr); code != 2 || !done {
			t.Errorf("%v: code %d done %v (%s)", args, code, done, stderr.String())
		}
	}
	if o, code, _ := parseArgs([]string{"--out", "f", "--path", "x"}, io.Discard, io.Discard); code != 0 || o.out != "f" {
		t.Errorf("valid --out: %+v %d", o, code)
	}
}
