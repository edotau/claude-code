package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepGoCacheOverCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOCACHE", dir)
	if err := os.WriteFile(filepath.Join(dir, "x"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	old := GoCacheCap
	t.Cleanup(func() { GoCacheCap = old })
	GoCacheCap = 1024
	var out strings.Builder
	tot := sweepTotals{}
	sweepGoCache(&out, false, &tot)
	if tot.files != 1 || !strings.Contains(out.String(), "would remove") {
		t.Fatalf("over cap dry run: %+v %q", tot, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err != nil {
		t.Fatalf("dry run deleted: %v", err)
	}
	GoCacheCap = 1 << 20
	tot = sweepTotals{}
	sweepGoCache(&out, true, &tot)
	if tot.files != 0 {
		t.Fatalf("under cap swept: %+v", tot)
	}
}
