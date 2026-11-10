package memory

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSplitNativeIndexMovesOldestUnpinnedOverflow: file order is oldest-first (CC appends new
// memories at the bottom), so the split must take the FRONT of the unpinned run, keeping the
// newest within the cap.
func TestSplitNativeIndexMovesOldestUnpinnedOverflow(t *testing.T) {
	tempConfig(t)
	dir := NativeDir(t.TempDir())
	var b strings.Builder
	b.WriteString("# Memory Index\n\n")
	for i := 1; i <= 5; i++ {
		n := strconv.Itoa(i)
		fmt.Fprintf(&b, "- [Rule %s](rule-%s.md) — hook\n", n, n)
	}
	write(t, filepath.Join(dir, NativeIndexFile), b.String())

	moved, err := SplitNativeIndex(dir, 3)
	if err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	for _, keep := range []string{"rule-3.md", "rule-4.md", "rule-5.md"} {
		if !strings.Contains(idx, keep) {
			t.Errorf("MEMORY.md lost a line it should have kept (%s):\n%s", keep, idx)
		}
	}
	searchable := mustRead(t, filepath.Join(dir, NativeSearchableFile))
	for _, name := range []string{"rule-1.md", "rule-2.md"} {
		if !strings.Contains(searchable, name) {
			t.Errorf("SEARCHABLE.md missing the moved line (%s):\n%s", name, searchable)
		}
	}
	if strings.Contains(idx, "rule-1.md") || strings.Contains(idx, "rule-2.md") {
		t.Errorf("MEMORY.md still carries a moved line:\n%s", idx)
	}
}

// TestSplitNativeIndexKeepPinSurvives: a `<!-- keep -->` marker holds a line in MEMORY.md even
// when it is the oldest entry and would otherwise be first to move.
func TestSplitNativeIndexKeepPinSurvives(t *testing.T) {
	tempConfig(t)
	dir := NativeDir(t.TempDir())
	index := "# Memory Index\n\n" +
		"- [Pinned](pinned.md) — load-bearing <!-- keep -->\n" +
		"- [Rule 2](rule-2.md) — hook\n" +
		"- [Rule 3](rule-3.md) — hook\n" +
		"- [Rule 4](rule-4.md) — hook\n"
	write(t, filepath.Join(dir, NativeIndexFile), index)

	moved, err := SplitNativeIndex(dir, 2)
	if err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2 (budget 2 minus 1 pin leaves room for 1 unpinned)", moved)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "pinned.md") {
		t.Error("the pinned line was moved — a `<!-- keep -->` marker must hold it in MEMORY.md")
	}
	if !strings.Contains(idx, "rule-4.md") {
		t.Error("the newest unpinned line should survive within budget")
	}
	if strings.Contains(idx, "rule-2.md") || strings.Contains(idx, "rule-3.md") {
		t.Errorf("overflow unpinned lines were not moved:\n%s", idx)
	}
}

// TestSplitNativeIndexNoopUnderCap: a MEMORY.md already within the cap is left untouched.
func TestSplitNativeIndexNoopUnderCap(t *testing.T) {
	dir := nativeStore(t)
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))
	moved, err := SplitNativeIndex(dir, NativeCoreCap)
	if err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
	if got := mustRead(t, filepath.Join(dir, NativeIndexFile)); got != before {
		t.Error("a store already under cap must not be rewritten")
	}
	if fileExists(filepath.Join(dir, NativeSearchableFile)) {
		t.Error("nothing should have been moved into SEARCHABLE.md")
	}
}

// TestNativeDupesFindsOverlappingBodies: two topic files with near-identical bodies cluster, and
// the longer body survives as Keep.
func TestNativeDupesFindsOverlappingBodies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shared := "the deploy pipeline retries three times before paging oncall about the failure"
	write(t, filepath.Join(dir, "short.md"), "---\nname: short\n---\n\n"+shared+"\n")
	write(t, filepath.Join(dir, "long.md"), "---\nname: long\n---\n\n"+shared+" and then escalates to the secondary rotation\n")

	clusters := NativeDupes(dir, 0.6)
	if len(clusters) != 1 {
		t.Fatalf("clusters = %+v, want exactly 1", clusters)
	}
	if clusters[0].Keep != "long.md" {
		t.Errorf("Keep = %q, want long.md (the longer body)", clusters[0].Keep)
	}
	if len(clusters[0].Losers) != 1 || clusters[0].Losers[0] != "short.md" {
		t.Errorf("Losers = %v, want [short.md]", clusters[0].Losers)
	}
}
