package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testIndex = `# Memory Index

- [Keep me](keep-me.md) — a live preference
- [Stale fact](stale-fact.md) — a closed incident
- [Also keep](also-keep.md) — another live one
`

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// nativeStore seeds a native auto-memory dir (MEMORY.md + topic files) and returns it.
func nativeStore(t *testing.T) string {
	t.Helper()
	tempConfig(t)
	dir := NativeDir(t.TempDir())
	write(t, filepath.Join(dir, NativeIndexFile), testIndex)
	for _, n := range []string{"keep-me.md", "stale-fact.md", "also-keep.md"} {
		write(t, filepath.Join(dir, n), "---\nname: "+strings.TrimSuffix(n, ".md")+"\n---\n\nbody\n")
	}
	return dir
}

// TestArchiveNativeMovesFileAndStrikesIndexLine is the core contract: the topic file leaves the
// injected surface for the archive, and its MEMORY.md line goes with it.
func TestArchiveNativeMovesFileAndStrikesIndexLine(t *testing.T) {
	dir := nativeStore(t)
	res, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "stale-fact.md" {
		t.Fatalf("archived %v, want [stale-fact.md]", res.Archived)
	}
	if fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("topic file still in the injected store — the move did not retire the original")
	}
	if !fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("topic file missing from the archive")
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if strings.Contains(idx, "stale-fact.md") {
		t.Errorf("index line survived the archive:\n%s", idx)
	}
	for _, keep := range []string{"keep-me.md", "also-keep.md"} {
		if !strings.Contains(idx, keep) {
			t.Errorf("archive struck an unrelated index line (%s):\n%s", keep, idx)
		}
	}
	// The struck line is recorded verbatim so a restore is exact rather than reconstructed.
	rec := readFile(t, filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile))
	if !strings.Contains(rec, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("archive index did not record the original line verbatim:\n%s", rec)
	}
}

// TestArchiveNativeDryRunWritesNothing: the default invocation reports and writes nothing.
func TestArchiveNativeDryRunWritesNothing(t *testing.T) {
	dir := nativeStore(t)
	before := readFile(t, filepath.Join(dir, NativeIndexFile))
	res, err := ArchiveNative(dir, []string{"stale-fact"}, false)
	if err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	if len(res.Archived) != 1 {
		t.Fatalf("dry run planned %v, want 1 entry", res.Archived)
	}
	if fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("dry run wrote into the archive")
	}
	if !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("dry run moved the topic file")
	}
	if got := readFile(t, filepath.Join(dir, NativeIndexFile)); got != before {
		t.Error("dry run rewrote MEMORY.md")
	}
}

// TestArchiveNativeRefusesIndexFilesAndTraversal: MEMORY.md/SEARCHABLE.md/INDEX.md share the
// directory with topic files; a traversal or a .md-suffixed directory passes a naive name check.
func TestArchiveNativeRefusesIndexFilesAndTraversal(t *testing.T) {
	dir := nativeStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "subtree.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{NativeIndexFile, NativeSearchableFile, NativeArchiveIndexFile,
		"../escape", "nested/topic.md", "subtree.md", ""} {
		if _, err := ArchiveNative(dir, []string{name}, true); err == nil {
			t.Errorf("ArchiveNative(%q) was allowed; want refusal", name)
		}
	}
	// A name that simply is not there is SKIPPED, not an error — archiving is idempotent by design.
	res, err := ArchiveNative(dir, []string{"never-existed"}, true)
	if err != nil {
		t.Fatalf("an absent name should skip, not fail: %v", err)
	}
	if len(res.Archived) != 0 || len(res.Skipped) != 1 {
		t.Errorf("archived %v / skipped %v, want 0 archived and 1 skipped", res.Archived, res.Skipped)
	}
}

// TestRestoreNativeRoundTrip: archive -> restore returns both the file and its index line.
func TestRestoreNativeRoundTrip(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	res, err := RestoreNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("RestoreNative: %v", err)
	}
	if len(res.Restored) != 1 {
		t.Fatalf("restored %v, want 1", res.Restored)
	}
	if !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("topic file not back in the store")
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("index line not restored verbatim:\n%s", idx)
	}
	if fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("restore left a copy in the archive")
	}
}

// TestNativeStatsCountsOnlyTopicFiles: MEMORY.md itself must not inflate the reported topic count.
func TestNativeStatsCountsOnlyTopicFiles(t *testing.T) {
	dir := nativeStore(t)
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatalf("NativeStats: %v", err)
	}
	if st.TopicFiles != 3 {
		t.Errorf("TopicFiles = %d, want 3 (MEMORY.md excluded)", st.TopicFiles)
	}
	if st.IndexEntries != 3 {
		t.Errorf("IndexEntries = %d, want 3", st.IndexEntries)
	}
}

// TestNativeStatsFindsOrphansAndDangling: a topic no pointer names, and a pointer naming no topic.
func TestNativeStatsFindsOrphansAndDangling(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, NativeIndexFile), "# Index\n\n- [Gone](gone.md) — dangling\n")
	write(t, filepath.Join(dir, "orphan.md"), "---\nname: orphan\n---\nbody\n")
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatalf("NativeStats: %v", err)
	}
	if len(st.Dangling) != 1 || st.Dangling[0] != "gone.md" {
		t.Errorf("Dangling = %v, want [gone.md]", st.Dangling)
	}
	found := false
	for _, o := range st.Orphans {
		found = found || o == "orphan.md"
	}
	if !found {
		t.Errorf("Orphans = %v, want to include orphan.md", st.Orphans)
	}
}

// TestReconcileHealsCrashedRestorePointer: simulate a crash between a move and its index write —
// the file is back in the store, MEMORY.md still lacks the pointer, and the archive's own record
// still has it. ArchiveNative (which reconciles before archiving) must heal it, verbatim.
func TestReconcileHealsCrashedRestorePointer(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	// Simulate the crash: move the file back by hand, without touching either index.
	if err := os.Rename(filepath.Join(NativeArchiveDir(dir), "stale-fact.md"), filepath.Join(dir, "stale-fact.md")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, NativeIndexFile)), "stale-fact.md") {
		t.Fatal("test setup: MEMORY.md must not already point at the crashed-back file")
	}

	if _, err := ArchiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("ArchiveNative(keep-me): %v", err)
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("reconcile did not heal the crashed-restore pointer verbatim:\n%s", idx)
	}
}

// TestReconcileAddsFrontmatterPointerForUnindexedFile: a topic file present but never indexed (no
// archive record either) must be re-added FROM ITS OWN FRONTMATTER by the next locked op.
func TestReconcileAddsFrontmatterPointerForUnindexedFile(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "lost-one.md"), "---\nname: lost-one\ndescription: a crashed restore\n---\n\nbody\n")

	if _, err := ArchiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [lost-one](lost-one.md) — a crashed restore") {
		t.Errorf("reconcile did not add a frontmatter-built pointer:\n%s", idx)
	}
}

// TestReconcileSkipsFrontmatterlessTopics: a topic file with no closed frontmatter block has no
// name to point at, so it must not gain a spurious MEMORY.md line.
func TestReconcileSkipsFrontmatterlessTopics(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "no-frontmatter.md"), "just body text, no --- block\n")

	if _, err := ArchiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if strings.Contains(idx, "no-frontmatter.md") {
		t.Errorf("reconcile added a pointer for a frontmatterless topic file:\n%s", idx)
	}
}

// TestReconcileDoesNotRunOnDryRun: ArchiveNative's plan-only path must not reconcile, so an
// unindexed topic file present in the store must NOT gain a pointer on a dry run.
func TestReconcileDoesNotRunOnDryRun(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "lost-one.md"), "---\nname: lost-one\ndescription: a crashed restore\n---\n\nbody\n")
	before := readFile(t, filepath.Join(dir, NativeIndexFile))

	if _, err := ArchiveNative(dir, []string{"keep-me"}, false); err != nil {
		t.Fatalf("ArchiveNative (dry run): %v", err)
	}
	if got := readFile(t, filepath.Join(dir, NativeIndexFile)); got != before {
		t.Errorf("dry run reconciled MEMORY.md:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestSearchableIndexIsIndexedButNotInjected: a pointer moved to SEARCHABLE.md counts as indexed —
// reconcile must not re-inject it, the audit sees no orphan, and archiving the topic strikes its
// line from SEARCHABLE.md, recording it verbatim.
func TestSearchableIndexIsIndexedButNotInjected(t *testing.T) {
	dir := nativeStore(t)
	idx := strings.Replace(testIndex, "- [Stale fact](stale-fact.md) — a closed incident\n", "", 1)
	write(t, filepath.Join(dir, NativeIndexFile), idx)
	write(t, filepath.Join(dir, NativeSearchableFile), "# Searchable\n\n- [Stale fact](stale-fact.md) — a closed incident\n")

	if err := ReconcileNative(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, NativeIndexFile)); strings.Contains(got, "stale-fact.md") {
		t.Fatalf("reconcile re-injected a SEARCHABLE.md topic into MEMORY.md:\n%s", got)
	}
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 0 || len(st.Dangling) != 0 || st.IndexEntries != 2 {
		t.Fatalf("stats = %+v, want 2 injected entries and no orphans/dangling", st)
	}

	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, NativeSearchableFile)); strings.Contains(got, "stale-fact.md") {
		t.Errorf("archive left the SEARCHABLE.md line:\n%s", got)
	}
	if rec := readFile(t, filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile)); !strings.Contains(rec, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("archive did not record the SEARCHABLE.md line verbatim:\n%s", rec)
	}
}

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
		b.WriteString("- [Rule " + n + "](rule-" + n + ".md) — hook\n")
	}
	write(t, filepath.Join(dir, NativeIndexFile), b.String())

	moved, err := SplitNativeIndex(dir, 3)
	if err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	for _, keep := range []string{"rule-3.md", "rule-4.md", "rule-5.md"} {
		if !strings.Contains(idx, keep) {
			t.Errorf("MEMORY.md lost a line it should have kept (%s):\n%s", keep, idx)
		}
	}
	searchable := readFile(t, filepath.Join(dir, NativeSearchableFile))
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
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
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
	before := readFile(t, filepath.Join(dir, NativeIndexFile))
	moved, err := SplitNativeIndex(dir, NativeCoreCap)
	if err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
	if got := readFile(t, filepath.Join(dir, NativeIndexFile)); got != before {
		t.Error("a store already under cap must not be rewritten")
	}
	if fileExists(filepath.Join(dir, NativeSearchableFile)) {
		t.Error("nothing should have been moved into SEARCHABLE.md")
	}
}

// TestNativeDupesFindsOverlappingBodies: two topic files with near-identical bodies cluster, and
// the longer body survives as Keep.
func TestNativeDupesFindsOverlappingBodies(t *testing.T) {
	tempConfig(t)
	dir := NativeDir(t.TempDir())
	shared := "the deploy pipeline retries three times before paging oncall about the failure"
	write(t, filepath.Join(dir, "short.md"), "---\nname: short\n---\n\n"+shared+"\n")
	write(t, filepath.Join(dir, "long.md"), "---\nname: long\n---\n\n"+shared+" and then escalates to the secondary rotation\n")

	clusters, err := NativeDupes(dir, 0.6)
	if err != nil {
		t.Fatalf("NativeDupes: %v", err)
	}
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

// TestNativeArchiveDirIsPerProject: two stores with different slugs archive into different dirs,
// so one project's archive never collides with another's.
func TestNativeArchiveDirIsPerProject(t *testing.T) {
	tempConfig(t)
	a := NativeDir(filepath.Join(t.TempDir(), "repo-a"))
	b := NativeDir(filepath.Join(t.TempDir(), "repo-b"))
	if NativeArchiveDir(a) == NativeArchiveDir(b) {
		t.Errorf("both projects archive into %s", NativeArchiveDir(a))
	}
}
