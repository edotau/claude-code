package transcript

import (
	"os"
	"strings"
	"testing"
)

// BenchmarkScan runs the per-Stop scans over a real transcript; set HARNESS_BENCH_TRANSCRIPT to one.
func BenchmarkScan(b *testing.B) {
	path := os.Getenv("HARNESS_BENCH_TRANSCRIPT")
	if path == "" {
		b.Skip("HARNESS_BENCH_TRANSCRIPT unset")
	}
	b.ReportAllocs()
	for b.Loop() {
		CountEdits(path)
		EditedFiles(path, 50)
	}
}

func TestEachLineOverflowAndUnterminated(t *testing.T) {
	giant := strings.Repeat("x", 3<<20) + "\n"
	in := "a\n" + giant + "b\nlast"
	var got []string
	eachLine(strings.NewReader(in), func(l []byte) { got = append(got, string(l)) })
	want := []string{"a\n", giant, "b\n", "last"}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: len %d, want len %d", i, len(got[i]), len(want[i]))
		}
	}
}
