package memory

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRestoreNativeRoundTrip: archive -> restore returns both the file and its index line.
func TestRestoreNativeRoundTrip(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	res, err := restoreNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("restoreNative: %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("restored %v, want 1", res.Restored)
	}
	if !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("topic file not back in the store")
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("index line not restored verbatim:\n%s", idx)
	}
	if fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("restore left a copy in the archive")
	}
}

// TestRestoreNativeRefusesDifferingLiveFile: a live file with other bytes is never replaced; nothing
// moves and neither index changes.
func TestRestoreNativeRefusesDifferingLiveFile(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), "---\nname: stale-fact\n---\n\nrewritten live\n")
	idxBefore := mustRead(t, filepath.Join(dir, NativeIndexFile))
	recBefore := mustRead(t, filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile))
	_, err := restoreNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "differs from the archived copy") {
		t.Fatalf("restoreNative err = %v, want a differing-live-file refusal", err)
	}
	if !fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("archive copy moved despite the refusal")
	}
	if got := mustRead(t, filepath.Join(dir, "stale-fact.md")); !strings.Contains(got, "rewritten live") {
		t.Error("live file was replaced")
	}
	if mustRead(t, filepath.Join(dir, NativeIndexFile)) != idxBefore {
		t.Error("MEMORY.md changed on a refused restore")
	}
	if mustRead(t, filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile)) != recBefore {
		t.Error("archive record changed on a refused restore")
	}
}

// TestRestoreNativeDedupesEqualLiveFile: an identical live copy (a crashed restore) is kept, the archive
// copy is dropped, and the pointer is present exactly once.
func TestRestoreNativeDedupesEqualLiveFile(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), mustRead(t, filepath.Join(nativeArchiveDir(dir), "stale-fact.md")))
	res, err := restoreNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("restoreNative: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "stale-fact.md" {
		t.Errorf("Restored = %v, want [stale-fact.md]", res.Restored)
	}
	if fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("archive copy survived the dedupe")
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if n := strings.Count(idx, "- [Stale fact](stale-fact.md) — a closed incident"); n != 1 {
		t.Errorf("pointer present %d times, want 1:\n%s", n, idx)
	}
	if rec := mustRead(t, filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile)); strings.Contains(rec, "stale-fact.md") {
		t.Errorf("archive record still carries the restored line:\n%s", rec)
	}
}

// TestRestoreNativeRefusesReservedNameEvenIfArchived: a reserved name sitting in the archive
// (e.g. hand-placed) still cannot be restored — the name check runs before the archive lookup.
func TestRestoreNativeRefusesReservedNameEvenIfArchived(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	archiveDir := nativeArchiveDir(dir)
	write(t, filepath.Join(archiveDir, "activeContext.md"), "focus\n")
	if _, err := restoreNative(dir, []string{"activeContext.md"}, true); err == nil {
		t.Error("restoreNative(activeContext.md) was allowed because a file of that name sat in the archive")
	}
}

// TestSplitReportsNativeProseOverLimit: the split moves what it can and then reports an index the native loader
// would still truncate instead of silently rewriting the prose that busts it.
func TestSplitReportsNativeProseOverLimit(t *testing.T) {
	dir := nativeStore(t)
	indexPath := filepath.Join(dir, NativeIndexFile)
	prose := strings.Repeat("prose that is not a pointer\n", NativeIndexMaxLines)
	write(t, indexPath, prose+testIndex)
	moved, err := splitNativeIndex(dir, 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("err = %v, want the native load-limit diagnostic", err)
	}
	got := mustRead(t, indexPath)
	if !strings.HasPrefix(got, prose) || strings.Contains(got, "stale-fact.md") || !strings.Contains(got, "also-keep.md") {
		t.Errorf("split rewrote prose or kept the wrong pointers:\n%s", got[len(got)-200:])
	}
	if _, err := splitNativeIndex(dir, 1); err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("no-op re-run err = %v, want the same diagnostic", err)
	}
}

// TestRestoreNativeReportsIndexOverLimit: a restore whose pointer pushes MEMORY.md past the native load
// limit still lands, and the error names the limit so the overflow is never silent.
func TestRestoreNativeReportsIndexOverLimit(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(dir, NativeIndexFile)
	idx := mustRead(t, idxPath)
	write(t, idxPath, idx+strings.Repeat("prose\n", NativeIndexMaxLines-lineCount(idx)))
	res, err := restoreNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "exceed the native load limit") {
		t.Fatalf("restoreNative err = %v, want the load-limit diagnostic", err)
	}
	if len(res.Restored) != 1 || !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Errorf("restore did not land: Restored = %v", res.Restored)
	}
	if got := mustRead(t, idxPath); !strings.Contains(got, "(stale-fact.md)") || lineCount(got) != NativeIndexMaxLines+1 {
		t.Errorf("index has %d lines, want %d with the restored pointer", lineCount(got), NativeIndexMaxLines+1)
	}
}

// TestSplitNativeIndexMovesOldestUnpinnedOverflow: file order is oldest-first (CC appends new
// memories at the bottom), so the split must take the FRONT of the unpinned run, keeping the
// newest within the cap.
func TestSplitNativeIndexMovesOldestUnpinnedOverflow(t *testing.T) {
	tempConfig(t)
	dir := Dir(t.TempDir())
	var b strings.Builder
	b.WriteString("# Memory Index\n\n")
	for i := 1; i <= 5; i++ {
		n := strconv.Itoa(i)
		fmt.Fprintf(&b, "- [Rule %s](rule-%s.md) — hook\n", n, n)
	}
	write(t, filepath.Join(dir, NativeIndexFile), b.String())

	moved, err := splitNativeIndex(dir, 3)
	if err != nil {
		t.Fatalf("splitNativeIndex: %v", err)
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
	dir := Dir(t.TempDir())
	index := "# Memory Index\n\n" +
		"- [Pinned](pinned.md) — load-bearing <!-- keep -->\n" +
		"- [Rule 2](rule-2.md) — hook\n" +
		"- [Rule 3](rule-3.md) — hook\n" +
		"- [Rule 4](rule-4.md) — hook\n"
	write(t, filepath.Join(dir, NativeIndexFile), index)

	moved, err := splitNativeIndex(dir, 2)
	if err != nil {
		t.Fatalf("splitNativeIndex: %v", err)
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
	moved, err := splitNativeIndex(dir, NativeCoreCap)
	if err != nil {
		t.Fatalf("splitNativeIndex: %v", err)
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
