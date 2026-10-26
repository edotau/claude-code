package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// TestTopicFrontmatterFencesAndRawValues: topicFrontmatter's closing fence must start its own line
// exactly ("\n---\n", per markdown.StripFrontMatter's own contract) — a trailing-space fence, an
// indented fence, or a missing final newline is therefore unclosed frontmatter (ok=false, body text).
// A well-formed fence never coerces a scalar to another YAML type (the type-coercion regression this
// reverts): `yes`, `[draft]` and `007` all come back as the literal raw text.
func TestTopicFrontmatterFencesAndRawValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, wantName, wantDesc string
		wantOK                        bool
	}{
		{"trailing-space fence is unclosed (strict \\n---\\n)", "---  \nname: a\n---  \nbody", "", "", false},
		{"indented fence is unclosed (strict ---\\n opener)", "  ---\nname: a\n  ---\nbody", "", "", false},
		{"no final newline is unclosed (strict \\n---\\n closer)", "---\nname: a\n---", "", "", false},
		{"name: yes stays raw text, not a bool", "---\nname: yes\n---\nbody", "yes", "", true},
		{"description: [draft] stays raw text, not a list", "---\nname: x\ndescription: [draft]\n---\nbody", "x", "[draft]", true},
		{"name: 007 stays raw text, not a number", "---\nname: 007\n---\nbody", "007", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc, _, ok := topicFrontmatter(tc.raw)
			if ok != tc.wantOK || name != tc.wantName || desc != tc.wantDesc {
				t.Errorf("topicFrontmatter(%q) = name=%q desc=%q ok=%v, want name=%q desc=%q ok=%v",
					tc.raw, name, desc, ok, tc.wantName, tc.wantDesc, tc.wantOK)
			}
		})
	}
}

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

// plainNativeStore seeds an isolated native auto-memory dir without touching CLAUDE_CONFIG_DIR.
func plainNativeStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory")
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
	t.Parallel()
	dir := plainNativeStore(t)
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
	// classifyMemoryFile is directory-aware: SEARCHABLE.md/INDEX.md must exist to be classified and refused.
	write(t, filepath.Join(dir, NativeSearchableFile), "# Searchable\n")
	write(t, filepath.Join(dir, NativeArchiveIndexFile), "# Archived\n")
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
	t.Parallel()
	dir := plainNativeStore(t)
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
	t.Parallel()
	dir := plainNativeStore(t)
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
	t.Parallel()
	dir := plainNativeStore(t)
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
// the locked batch's reconcile must not re-inject it, the audit sees no orphan, and archiving the
// topic strikes its line from SEARCHABLE.md, recording it verbatim.
func TestSearchableIndexIsIndexedButNotInjected(t *testing.T) {
	dir := nativeStore(t)
	idx := strings.Replace(testIndex, "- [Stale fact](stale-fact.md) — a closed incident\n", "", 1)
	write(t, filepath.Join(dir, NativeIndexFile), idx)
	write(t, filepath.Join(dir, NativeSearchableFile), "# Searchable\n\n- [Stale fact](stale-fact.md) — a closed incident\n")

	if _, err := ArchiveNative(dir, []string{"also-keep"}, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, NativeIndexFile)); strings.Contains(got, "stale-fact.md") {
		t.Fatalf("reconcile re-injected a SEARCHABLE.md topic into MEMORY.md:\n%s", got)
	}
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 0 || len(st.Dangling) != 0 || st.IndexEntries != 1 {
		t.Fatalf("stats = %+v, want 1 injected entry and no orphans/dangling", st)
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
	t.Parallel()
	dir := t.TempDir()
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
	a := NativeDir(repoRoot(t, "repo-a"))
	b := NativeDir(repoRoot(t, "repo-b"))
	if NativeArchiveDir(a) == NativeArchiveDir(b) {
		t.Errorf("both projects archive into %s", NativeArchiveDir(a))
	}
}

// bankRegion is a harness-generated MEMORY.md region pointing at a typed bank file.
const bankRegion = "<!-- bank:begin generated by claude-code memory index -->\n" +
	"- [Active context](activeContext.md) — current focus\n" +
	"<!-- bank:end -->\n"

// TestArchiveNativeRefusesSymlinkAndSkipsNonTopics: a symlinked .md is refused for mutation, and the
// catalog, .origin and typed files are neither topics nor orphans.
func TestArchiveNativeRefusesSymlinkAndSkipsNonTopics(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "activeContext.md"), "# Active\n")
	write(t, filepath.Join(dir, "custom.md"), "---\nname: custom\nharness_kind: bank\n---\nbody\n")
	write(t, filepath.Join(dir, CatalogFile), "# catalog\n")
	write(t, filepath.Join(dir, legacyOriginFile), "{}\n")
	if err := os.Symlink(filepath.Join(dir, "keep-me.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveNative(dir, []string{"link"}, true); err == nil || !fileExists(filepath.Join(dir, "link.md")) {
		t.Errorf("ArchiveNative(link) err = %v; want refusal with the symlink left in place", err)
	}
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.TopicFiles != 3 {
		t.Errorf("TopicFiles = %d, want 3 (bank/catalog/origin/symlink excluded)", st.TopicFiles)
	}
	for _, o := range st.Orphans {
		if o == "activeContext.md" || o == "custom.md" {
			t.Errorf("bank file reported as an orphan: %v", st.Orphans)
		}
	}
}

// TestBankRegionPointerToAbsentFileIsNotDangling: region pointers are never parsed, so one naming an
// absent typed file is neither counted nor reported dangling, and only real topics are orphans.
func TestBankRegionPointerToAbsentFileIsNotDangling(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	write(t, filepath.Join(dir, NativeIndexFile), "# Memory Index\n\n"+bankRegion+"\n"+
		"- [Keep me](keep-me.md) — a live preference\n- [Stale fact](stale-fact.md) — a closed incident\n")
	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.IndexEntries != 2 || len(st.Dangling) != 0 {
		t.Errorf("IndexEntries = %d, Dangling = %v; want 2 and none", st.IndexEntries, st.Dangling)
	}
	if len(st.Orphans) != 1 || st.Orphans[0] != "also-keep.md" {
		t.Errorf("Orphans = %v, want [also-keep.md]", st.Orphans)
	}
}

// TestArchiveNativeVersionsCollidingRevision: a second archive of the same name with different bytes
// lands beside the first under a stamped name; a same-second third one gets a -2 suffix.
func TestArchiveNativeVersionsCollidingRevision(t *testing.T) {
	dir := nativeStore(t)
	archive := NativeArchiveDir(dir)
	first := readFile(t, filepath.Join(dir, "stale-fact.md"))
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), first+"\nrevised\n")
	res, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || !strings.HasPrefix(res.Archived[0], "stale-fact@") || !strings.HasSuffix(res.Archived[0], ".md") {
		t.Fatalf("Archived = %v, want one stamped stale-fact@<utc>.md", res.Archived)
	}
	if got := readFile(t, filepath.Join(archive, "stale-fact.md")); got != first {
		t.Errorf("the earlier revision was replaced: %q", got)
	}
	if got := readFile(t, filepath.Join(archive, res.Archived[0])); got != first+"\nrevised\n" {
		t.Errorf("stamped copy = %q", got)
	}
	// A third revision in the same second gets a -2 suffix rather than replacing the stamped copy.
	write(t, filepath.Join(dir, "stale-fact.md"), first+"\nrevised again\n")
	third, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Archived) != 1 || third.Archived[0] == res.Archived[0] || !fileExists(filepath.Join(archive, res.Archived[0])) {
		t.Errorf("third archive = %v, want a distinct version beside %s", third.Archived, res.Archived[0])
	}
}

// TestRestoreNativeRefusesDifferingLiveFile: a live file with other bytes is never replaced; nothing
// moves and neither index changes.
func TestRestoreNativeRefusesDifferingLiveFile(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), "---\nname: stale-fact\n---\n\nrewritten live\n")
	idxBefore := readFile(t, filepath.Join(dir, NativeIndexFile))
	recBefore := readFile(t, filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile))
	_, err := RestoreNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "differs from the archived copy") {
		t.Fatalf("RestoreNative err = %v, want a differing-live-file refusal", err)
	}
	if !fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("archive copy moved despite the refusal")
	}
	if got := readFile(t, filepath.Join(dir, "stale-fact.md")); !strings.Contains(got, "rewritten live") {
		t.Error("live file was replaced")
	}
	if readFile(t, filepath.Join(dir, NativeIndexFile)) != idxBefore {
		t.Error("MEMORY.md changed on a refused restore")
	}
	if readFile(t, filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile)) != recBefore {
		t.Error("archive record changed on a refused restore")
	}
}

// TestRestoreNativeDedupesEqualLiveFile: an identical live copy (a crashed restore) is kept, the archive
// copy is dropped, and the pointer is present exactly once.
func TestRestoreNativeDedupesEqualLiveFile(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), readFile(t, filepath.Join(NativeArchiveDir(dir), "stale-fact.md")))
	res, err := RestoreNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("RestoreNative: %v", err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "stale-fact.md" {
		t.Errorf("Restored = %v, want [stale-fact.md]", res.Restored)
	}
	if fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("archive copy survived the dedupe")
	}
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if n := strings.Count(idx, "- [Stale fact](stale-fact.md) — a closed incident"); n != 1 {
		t.Errorf("pointer present %d times, want 1:\n%s", n, idx)
	}
	if rec := readFile(t, filepath.Join(NativeArchiveDir(dir), NativeArchiveIndexFile)); strings.Contains(rec, "stale-fact.md") {
		t.Errorf("archive record still carries the restored line:\n%s", rec)
	}
}

// TestArchiveAndRestoreRefuseReservedAndBankNames: every reserved/typed name is refused with an
// error naming its kind, for both ArchiveNative and RestoreNative.
func TestArchiveAndRestoreRefuseReservedAndBankNames(t *testing.T) {
	dir := nativeStore(t)
	archiveDir := NativeArchiveDir(dir)
	names := append(append([]string{}, bankFiles...),
		NativeIndexFile, NativeSearchableFile, NativeArchiveIndexFile, "index.md", "typed-notes.md")
	for _, n := range names {
		body := "bank body\n"
		if n == "typed-notes.md" {
			body = "---\nname: typed-notes\nharness_kind: bank\n---\n\nbody\n"
		}
		write(t, filepath.Join(dir, n), body)
		write(t, filepath.Join(archiveDir, n), body)
	}
	for _, n := range names {
		if _, err := ArchiveNative(dir, []string{n}, true); err == nil {
			t.Errorf("ArchiveNative(%q) was allowed; want refusal naming the kind", n)
		}
		if _, err := RestoreNative(dir, []string{n}, true); err == nil {
			t.Errorf("RestoreNative(%q) was allowed; want refusal naming the kind", n)
		}
	}
}

// TestTopicPathsExcludeMetadataBankAndSymlink: a harness_kind:bank topic and a symlinked .md are
// not topics — topicPaths, NativeStats.TopicFiles and NativeDupes must all exclude them.
func TestTopicPathsExcludeMetadataBankAndSymlink(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	write(t, filepath.Join(dir, "typed-notes.md"), "---\nname: typed-notes\nharness_kind: bank\n---\n\n"+
		"the deploy pipeline retries three times before paging oncall\n")
	write(t, filepath.Join(dir, "real.md"), "---\nname: real\n---\n\n"+
		"the deploy pipeline retries three times before paging oncall\n")
	if err := os.Symlink(filepath.Join(dir, "real.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}

	excluded := []string{"typed-notes.md", "link.md"}
	var topicNames []string
	for _, p := range topicPaths(dir) {
		topicNames = append(topicNames, filepath.Base(p))
	}
	assertExcludesNames(t, topicNames, excluded, "topicPaths")

	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	// nativeStore seeds 3 topics; +real.md = 4. typed-notes.md and link.md excluded.
	if st.TopicFiles != 4 {
		t.Errorf("TopicFiles = %d, want 4 (metadata-bank file and symlink excluded)", st.TopicFiles)
	}

	clusters, err := NativeDupes(dir, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	var clustered []string
	for _, c := range clusters {
		clustered = append(append(clustered, c.Keep), c.Losers...)
	}
	assertExcludesNames(t, clustered, excluded, "NativeDupes")
}

// assertExcludesNames fails if any of excluded appears in got; context names the caller for the message.
func assertExcludesNames(t *testing.T, got, excluded []string, context string) {
	t.Helper()
	for _, n := range got {
		for _, x := range excluded {
			if n == x {
				t.Errorf("%s considered a non-topic file: %s", context, n)
			}
		}
	}
}

// TestBankRegionNeverTouchedByIndexOpsOrArchive: MEMORY.md's generated bank region (two pointer
// lines) is ignored by IndexEntries, never moved by SplitNativeIndex, and never struck by archive.
func TestBankRegionNeverTouchedByIndexOpsOrArchive(t *testing.T) {
	dir := nativeStore(t)
	region := "<!-- bank:begin generated by claude-code memory index -->\n" +
		"- [Active](activeContext.md) — focus\n- [Progress](progress.md) — status\n" +
		"<!-- bank:end -->\n"
	before := readFile(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+region)
	withRegion := readFile(t, filepath.Join(dir, NativeIndexFile))
	start, end := bankRegionBounds(withRegion)
	if start < 0 || end < 0 {
		t.Fatalf("test setup: bank region not detected in:\n%s", withRegion)
	}
	regionBytes := withRegion[start:end]

	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.IndexEntries != 3 {
		t.Errorf("IndexEntries = %d, want 3 (bank region's 2 pointers excluded)", st.IndexEntries)
	}

	if _, err := SplitNativeIndex(dir, 0); err != nil {
		t.Fatalf("SplitNativeIndex: %v", err)
	}
	assertRegionUnchanged(t, filepath.Join(dir, NativeIndexFile), regionBytes, "SplitNativeIndex")

	if _, err := ArchiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("ArchiveNative: %v", err)
	}
	assertRegionUnchanged(t, filepath.Join(dir, NativeIndexFile), regionBytes, "ArchiveNative")
}

// assertRegionUnchanged fails if path's bank region is missing or no longer equals want; op names the caller for the message.
func assertRegionUnchanged(t *testing.T, path, want, op string) {
	t.Helper()
	got := readFile(t, path)
	start, end := bankRegionBounds(got)
	if start < 0 || end < 0 || got[start:end] != want {
		t.Errorf("%s moved or altered the bank region:\n%s", op, got)
	}
}

// TestBankPointerToPresentTypedFileIsNotDanglingOrOrphan: a pointer naming a six-typed file that
// is actually present must not be flagged Dangling (the file exists) nor can that file be an
// Orphan (typed files are never topics, so they are outside the orphan check entirely).
func TestBankPointerToPresentTypedFileIsNotDanglingOrOrphan(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	write(t, filepath.Join(dir, "activeContext.md"), "focus\n")
	before := readFile(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+"- [Active](activeContext.md) — focus\n")

	st, err := NativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Dangling {
		if d == "activeContext.md" {
			t.Error("activeContext.md flagged Dangling while the file is present")
		}
	}
	for _, o := range st.Orphans {
		if o == "activeContext.md" {
			t.Error("activeContext.md flagged Orphan — typed files are never topics")
		}
	}
}

// TestArchiveCollisionVersionsByTimestampAndDedupesEqualContent: re-archiving a name whose
// archived copy has different bytes keeps BOTH under distinct names; identical bytes dedupe to
// one file; restoring onto a live file with different bytes is refused, leaving both intact.
func TestArchiveCollisionVersionsByTimestampAndDedupesEqualContent(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nversion one\n")
	before := readFile(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+"- [A](a.md) — v1\n")

	if _, err := ArchiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 1: %v", err)
	}
	// A fresh a.md with other bytes while the v1 copy still sits in the archive: the collision case.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nversion two\n")
	idx := readFile(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "a.md") {
		write(t, filepath.Join(dir, NativeIndexFile), idx+"- [A](a.md) — v2\n")
	}
	if _, err := ArchiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 2: %v", err)
	}
	archiveDir := NativeArchiveDir(dir)
	if got := archivedVersions(t, archiveDir, "a"); len(got) != 2 {
		t.Fatalf("archive entries = %v, want exactly a.md plus one a@<ts>.md", got)
	}

	// Equal-content re-archive must dedupe, not add a third version.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nversion two\n")
	idx2 := readFile(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), idx2+"- [A](a.md) — v2 again\n")
	if _, err := ArchiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 3 (equal content): %v", err)
	}
	if got := archivedVersions(t, archiveDir, "a"); len(got) != 2 {
		t.Fatalf("equal-content re-archive changed the version count: %v", got)
	}

	// Restore onto a live a.md with different bytes must fail, leaving both intact.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nlive divergent content\n")
	if _, err := RestoreNative(dir, []string{"a"}, true); err == nil {
		t.Error("RestoreNative onto a divergent live file was allowed; want refusal")
	}
	if !fileExists(filepath.Join(dir, "a.md")) {
		t.Error("the live file was removed by a failed restore")
	}
	if got := archivedVersions(t, archiveDir, "a"); len(got) == 0 {
		t.Error("the archived file was removed by a failed restore")
	}
}

// archivedVersions is archiveDir's entries for stem: "<stem>.md" and any "<stem>@<ts>.md" revision.
func archivedVersions(t *testing.T, archiveDir, stem string) []string {
	t.Helper()
	ents, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.Name() == stem+".md" || strings.HasPrefix(e.Name(), stem+"@") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestRestoreNativeRefusesReservedNameEvenIfArchived: a reserved name sitting in the archive
// (e.g. hand-placed) still cannot be restored — the name check runs before the archive lookup.
func TestRestoreNativeRefusesReservedNameEvenIfArchived(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	archiveDir := NativeArchiveDir(dir)
	write(t, filepath.Join(archiveDir, "activeContext.md"), "focus\n")
	if _, err := RestoreNative(dir, []string{"activeContext.md"}, true); err == nil {
		t.Error("RestoreNative(activeContext.md) was allowed because a file of that name sat in the archive")
	}
}

// TestArchiveNativeReturnsErrBankBusy: while another fd holds BankLockPath(), ArchiveNative
// must return ErrBankBusy (wrapped), and no "<lock>.lock" file is created anywhere.
func TestArchiveNativeReturnsErrBankBusy(t *testing.T) {
	shortBankWait(t)
	dir := nativeStore(t)
	if err := os.MkdirAll(filepath.Dir(BankLockPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	ran, err := paths.TryFileLock(BankLockPath(), 0, func() error {
		_, archErr := ArchiveNative(dir, []string{"stale-fact"}, true)
		if !errors.Is(archErr, ErrBankBusy) {
			t.Errorf("ArchiveNative under contention = %v, want errors.Is ErrBankBusy", archErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer TryFileLock: %v", err)
	}
	if !ran {
		t.Fatal("outer TryFileLock did not acquire the lock")
	}
	if fileExists(BankLockPath() + ".lock") {
		t.Error("a <lock>.lock file was created; TryFileLock-style contention must not add one")
	}
}

// TestNativeOpsInsideStoreCommit: archive, restore and split on a store dir each land through the transaction —
// a memory-Git commit, a receipt, no journal left, a clean tree — and the archive lives at dir/archive/native.
func TestNativeOpsInsideStoreCommit(t *testing.T) {
	store, decoy := storeFixture(t)
	dir := filepath.Join(store, "repo")
	write(t, filepath.Join(dir, NativeIndexFile), testIndex)
	for _, n := range []string{"keep-me.md", "stale-fact.md", "also-keep.md"} {
		write(t, filepath.Join(dir, n), "---\nname: "+strings.TrimSuffix(n, ".md")+"\n---\n\nbody\n")
	}
	if dry, err := ArchiveNative(dir, []string{"stale-fact"}, false); err != nil || dry.Commit != "" || StoreRepo().Exists() {
		t.Fatalf("dry run = %+v, %v (repo exists %v): must write nothing", dry, err, StoreRepo().Exists())
	}
	res, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err != nil || res.Commit == "" {
		t.Fatalf("archive = %+v, %v: want a memory-Git commit", res, err)
	}
	if want := filepath.Join(dir, "archive", "native"); NativeArchiveDir(dir) != want || !fileExists(filepath.Join(want, "stale-fact.md")) {
		t.Fatalf("archive landed at %s, want %s", NativeArchiveDir(dir), want)
	}
	assertStoreSettled(t, res.Commit)
	res, err = RestoreNative(dir, []string{"stale-fact"}, true)
	if err != nil || res.Commit == "" || !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Fatalf("restore = %+v, %v: want a commit and the live file back", res, err)
	}
	assertStoreSettled(t, res.Commit)
	moved, err := SplitNativeIndex(dir, 1)
	if err != nil || moved != 2 {
		t.Fatalf("split = %d, %v", moved, err)
	}
	head, _ := StoreRepo().Head()
	if head == res.Commit {
		t.Fatal("split committed nothing")
	}
	assertStoreSettled(t, head)
	if got := receipts(t); len(got) != 3 {
		t.Fatalf("receipts = %v, want one per applied op", got)
	}
	assertDecoyUntouched(t, decoy)
}

// assertStoreSettled: HEAD is the reported commit, the tree is clean and no journal is pending.
func assertStoreSettled(t *testing.T, commit string) {
	t.Helper()
	if head, _ := StoreRepo().Head(); head != commit {
		t.Fatalf("HEAD %s != reported commit %s", head, commit)
	}
	if st, _ := StoreRepo().Status(); len(st) != 0 {
		t.Fatalf("store dirty after commit: %+v", st)
	}
	if got := journals(t); len(got) != 0 {
		t.Fatalf("journal left behind: %v", got)
	}
}

// TestMalformedBankRegionRefusesEveryIndexRewrite: a lone or doubled marker is a diagnostic, never a guess —
// split, archive, restore and stats all name MEMORY.md and leave every byte in place.
func TestMalformedBankRegionRefusesEveryIndexRewrite(t *testing.T) {
	dir := nativeStore(t)
	indexPath := filepath.Join(dir, NativeIndexFile)
	broken := readFile(t, indexPath) + "<!-- bank:begin generated by claude-code memory index -->\n" +
		"- [Active](activeContext.md) — focus\n" // no end marker
	write(t, indexPath, broken)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); !errors.Is(err, errBankRegion) {
		t.Errorf("ArchiveNative err = %v, want errBankRegion", err)
	}
	if _, err := SplitNativeIndex(dir, 0); !errors.Is(err, errBankRegion) {
		t.Errorf("SplitNativeIndex err = %v, want errBankRegion", err)
	}
	if _, err := NativeStats(dir); !errors.Is(err, errBankRegion) {
		t.Errorf("NativeStats err = %v, want errBankRegion", err)
	}
	if got := readFile(t, indexPath); got != broken {
		t.Errorf("a refused rewrite still changed %s:\n%s", NativeIndexFile, got)
	}
	if fileExists(filepath.Join(dir, NativeSearchableFile)) || fileExists(filepath.Join(NativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("a refused rewrite still wrote SEARCHABLE.md or moved the topic")
	}
	write(t, filepath.Join(NativeArchiveDir(dir), "old.md"), "---\nname: old\n---\n\nbody\n")
	if _, err := RestoreNative(dir, []string{"old"}, true); !errors.Is(err, errBankRegion) {
		t.Errorf("RestoreNative err = %v, want errBankRegion", err)
	}
	if fileExists(filepath.Join(dir, "old.md")) {
		t.Error("a refused restore still landed the topic")
	}
}

// TestSplitReportsNativeProseOverLimit: the split moves what it can and then reports an index the native loader
// would still truncate instead of silently rewriting the prose that busts it.
func TestSplitReportsNativeProseOverLimit(t *testing.T) {
	dir := nativeStore(t)
	indexPath := filepath.Join(dir, NativeIndexFile)
	prose := strings.Repeat("prose that is not a pointer\n", NativeIndexMaxLines)
	write(t, indexPath, prose+testIndex)
	moved, err := SplitNativeIndex(dir, 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	if err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("err = %v, want the native load-limit diagnostic", err)
	}
	got := readFile(t, indexPath)
	if !strings.HasPrefix(got, prose) || strings.Contains(got, "stale-fact.md") || !strings.Contains(got, "also-keep.md") {
		t.Errorf("split rewrote prose or kept the wrong pointers:\n%s", got[len(got)-200:])
	}
	if _, err := SplitNativeIndex(dir, 1); err == nil || !strings.Contains(err.Error(), "native load limit") {
		t.Errorf("no-op re-run err = %v, want the same diagnostic", err)
	}
}

// TestRestoreNativeReportsIndexOverLimit: a restore whose pointer pushes MEMORY.md past the native load
// limit still lands, and the error names the limit so the overflow is never silent.
func TestRestoreNativeReportsIndexOverLimit(t *testing.T) {
	dir := nativeStore(t)
	if _, err := ArchiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(dir, NativeIndexFile)
	idx := readFile(t, idxPath)
	write(t, idxPath, idx+strings.Repeat("prose\n", NativeIndexMaxLines-lineCount(idx)))
	res, err := RestoreNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "exceed the native load limit") {
		t.Fatalf("RestoreNative err = %v, want the load-limit diagnostic", err)
	}
	if len(res.Restored) != 1 || !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Errorf("restore did not land: Restored = %v", res.Restored)
	}
	if got := readFile(t, idxPath); !strings.Contains(got, "(stale-fact.md)") || lineCount(got) != NativeIndexMaxLines+1 {
		t.Errorf("index has %d lines, want %d with the restored pointer", lineCount(got), NativeIndexMaxLines+1)
	}
}

// TestArchiveNativeReportsHealedIndexOverLimit: the reconcile repair riding an archive can grow MEMORY.md past
// the native load limit; the archive lands and the error names the limit.
func TestArchiveNativeReportsHealedIndexOverLimit(t *testing.T) {
	dir := nativeStore(t)
	for _, n := range []string{"orphan-one.md", "orphan-two.md"} {
		write(t, filepath.Join(dir, n), "---\nname: "+strings.TrimSuffix(n, ".md")+"\n---\n\nbody\n")
	}
	idxPath := filepath.Join(dir, NativeIndexFile)
	write(t, idxPath, testIndex+strings.Repeat("prose\n", NativeIndexMaxLines-lineCount(testIndex)))
	res, err := ArchiveNative(dir, []string{"stale-fact"}, true)
	if err == nil || !strings.Contains(err.Error(), "exceed the native load limit") {
		t.Fatalf("ArchiveNative err = %v, want the load-limit diagnostic", err)
	}
	if len(res.Archived) != 1 || fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Errorf("archive did not land: Archived = %v", res.Archived)
	}
	got := readFile(t, idxPath)
	if strings.Contains(got, "(stale-fact.md)") || !strings.Contains(got, "(orphan-one.md)") || !strings.Contains(got, "(orphan-two.md)") {
		t.Errorf("index not struck and healed:\n%s", got)
	}
	if lineCount(got) != NativeIndexMaxLines+1 {
		t.Errorf("index has %d lines, want %d", lineCount(got), NativeIndexMaxLines+1)
	}
}
