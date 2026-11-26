package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSweepGoCacheOverCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOCACHE", dir)
	if err := os.WriteFile(filepath.Join(dir, "x"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-2 * goCacheMinAge)
	if err := os.Chtimes(filepath.Join(dir, "x"), mt, mt); err != nil {
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

func TestTrimGoCacheKeepsFreshAndMeta(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldA, oldB := mk("aa/a-d", 5*time.Hour), mk("bb/b-d", 4*time.Hour)
	fresh, readme := mk("cc/c-d", 0), mk("README", 9*time.Hour)
	freed, failed := trimGoCache(dir, 400, 150, time.Hour, true)
	if failed != 0 || freed != 200 {
		t.Fatalf("freed %d failed %d", freed, failed)
	}
	for _, p := range []string{oldA, oldB} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s kept", p)
		}
	}
	for _, p := range []string{fresh, readme} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
}
