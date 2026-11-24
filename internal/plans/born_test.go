package plans

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func localTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation(time.DateTime, s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func touch(t *testing.T, path string, at time.Time) {
	t.Helper()
	writeFile(t, path, "- [ ] 1.1 One\n")
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestPlanBornRecordsFirstWriteAndClampsToTheDay(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	plan, early := filepath.Join(dir, "2026-10-05-a.md"), filepath.Join(dir, "2026-10-05-b.md")
	first := localTime(t, "2026-10-05 09:00:00")
	touch(t, plan, first)
	touch(t, early, localTime(t, "2026-10-01 00:00:00"))
	if got := planBorn(plan, false); !got.Equal(first) {
		t.Fatalf("mtime fallback = %v, want %v", got, first)
	}
	if _, ok := recordedBorn(paths.CanonDeep(plan)); ok {
		t.Fatal("a non-recording read must not write a record")
	}
	planBorn(plan, true)
	touch(t, plan, first.Add(48*time.Hour))
	if got := planBorn(plan, true); !got.Equal(first) {
		t.Fatalf("recorded born = %v, want %v after a rewrite", got, first)
	}
	if got, day := planBorn(early, false), localTime(t, "2026-10-05 00:00:00"); !got.Equal(day) {
		t.Fatalf("born before the plan's day = %v, want %v", got, day)
	}
}

func TestWritePlanPinsBornOnlyAfterTheWriteLands(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	plan := filepath.Join(t.TempDir(), "2026-10-05-a.md")
	if err := os.Mkdir(plan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePlan(plan, []byte("x"), 0o600); err == nil {
		t.Fatal("replacing a directory should fail")
	}
	if _, ok := recordedBorn(paths.CanonDeep(plan)); ok {
		t.Fatal("a failed write must not pin a born time")
	}
	if err := os.Remove(plan); err != nil {
		t.Fatal(err)
	}
	if err := writePlan(plan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := recordedBorn(paths.CanonDeep(plan)); !ok {
		t.Fatal("a landed write pins a born time")
	}
}
