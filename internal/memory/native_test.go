package memory

import (
	"errors"
	"os"
	"path/filepath"
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

// nativeStore seeds a native auto-memory dir (MEMORY.md + topic files) and returns it.
func nativeStore(t *testing.T) string {
	t.Helper()
	tempConfig(t)
	dir := Dir(t.TempDir())
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
	res, err := archiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	if len(res.Archived) != 1 || res.Archived[0] != "stale-fact.md" {
		t.Fatalf("archived %v, want [stale-fact.md]", res.Archived)
	}
	if fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("topic file still in the injected store — the move did not retire the original")
	}
	if !fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("topic file missing from the archive")
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if strings.Contains(idx, "stale-fact.md") {
		t.Errorf("index line survived the archive:\n%s", idx)
	}
	for _, keep := range []string{"keep-me.md", "also-keep.md"} {
		if !strings.Contains(idx, keep) {
			t.Errorf("archive struck an unrelated index line (%s):\n%s", keep, idx)
		}
	}
	// The struck line is recorded verbatim so a restore is exact rather than reconstructed.
	rec := mustRead(t, filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile))
	if !strings.Contains(rec, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("archive index did not record the original line verbatim:\n%s", rec)
	}
}

// TestArchiveNativeDryRunWritesNothing: the default invocation reports and writes nothing.
func TestArchiveNativeDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))
	res, err := archiveNative(dir, []string{"stale-fact"}, false)
	if err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	if len(res.Archived) != 1 {
		t.Fatalf("dry run planned %v, want 1 entry", res.Archived)
	}
	if fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("dry run wrote into the archive")
	}
	if !fileExists(filepath.Join(dir, "stale-fact.md")) {
		t.Error("dry run moved the topic file")
	}
	if got := mustRead(t, filepath.Join(dir, NativeIndexFile)); got != before {
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
		if _, err := archiveNative(dir, []string{name}, true); err == nil {
			t.Errorf("archiveNative(%q) was allowed; want refusal", name)
		}
	}
	// A name that simply is not there is SKIPPED, not an error — archiving is idempotent by design.
	res, err := archiveNative(dir, []string{"never-existed"}, true)
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

// TestNativeStatsCountsOnlyTopicFiles: MEMORY.md itself must not inflate the reported topic count.
func TestNativeStatsCountsOnlyTopicFiles(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	st, err := nativeStats(dir)
	if err != nil {
		t.Fatalf("nativeStats: %v", err)
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
	st, err := nativeStats(dir)
	if err != nil {
		t.Fatalf("nativeStats: %v", err)
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
// still has it. archiveNative (which reconciles before archiving) must heal it, verbatim.
func TestReconcileHealsCrashedRestorePointer(t *testing.T) {
	dir := nativeStore(t)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	// Simulate the crash: move the file back by hand, without touching either index.
	if err := os.Rename(filepath.Join(nativeArchiveDir(dir), "stale-fact.md"), filepath.Join(dir, "stale-fact.md")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustRead(t, filepath.Join(dir, NativeIndexFile)), "stale-fact.md") {
		t.Fatal("test setup: MEMORY.md must not already point at the crashed-back file")
	}

	if _, err := archiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("archiveNative(keep-me): %v", err)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("reconcile did not heal the crashed-restore pointer verbatim:\n%s", idx)
	}
}

// TestReconcileAddsFrontmatterPointerForUnindexedFile: a topic file present but never indexed (no
// archive record either) must be re-added FROM ITS OWN FRONTMATTER by the next locked op.
func TestReconcileAddsFrontmatterPointerForUnindexedFile(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "lost-one.md"), "---\nname: lost-one\ndescription: a crashed restore\n---\n\nbody\n")

	if _, err := archiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "- [lost-one](lost-one.md) — a crashed restore") {
		t.Errorf("reconcile did not add a frontmatter-built pointer:\n%s", idx)
	}
}

// TestReconcileSkipsFrontmatterlessTopics: a topic file with no closed frontmatter block has no
// name to point at, so it must not gain a spurious MEMORY.md line.
func TestReconcileSkipsFrontmatterlessTopics(t *testing.T) {
	dir := nativeStore(t)
	write(t, filepath.Join(dir, "no-frontmatter.md"), "just body text, no --- block\n")

	if _, err := archiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if strings.Contains(idx, "no-frontmatter.md") {
		t.Errorf("reconcile added a pointer for a frontmatterless topic file:\n%s", idx)
	}
}

// TestReconcileDoesNotRunOnDryRun: archiveNative's plan-only path must not reconcile, so an
// unindexed topic file present in the store must NOT gain a pointer on a dry run.
func TestReconcileDoesNotRunOnDryRun(t *testing.T) {
	t.Parallel()
	dir := plainNativeStore(t)
	write(t, filepath.Join(dir, "lost-one.md"), "---\nname: lost-one\ndescription: a crashed restore\n---\n\nbody\n")
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))

	if _, err := archiveNative(dir, []string{"keep-me"}, false); err != nil {
		t.Fatalf("archiveNative (dry run): %v", err)
	}
	if got := mustRead(t, filepath.Join(dir, NativeIndexFile)); got != before {
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

	if _, err := archiveNative(dir, []string{"also-keep"}, true); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dir, NativeIndexFile)); strings.Contains(got, "stale-fact.md") {
		t.Fatalf("reconcile re-injected a SEARCHABLE.md topic into MEMORY.md:\n%s", got)
	}
	assertSearchableIndexStats(t, dir)

	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(dir, NativeSearchableFile)); strings.Contains(got, "stale-fact.md") {
		t.Errorf("archive left the SEARCHABLE.md line:\n%s", got)
	}
	if rec := mustRead(t, filepath.Join(nativeArchiveDir(dir), NativeArchiveIndexFile)); !strings.Contains(rec, "- [Stale fact](stale-fact.md) — a closed incident") {
		t.Errorf("archive did not record the SEARCHABLE.md line verbatim:\n%s", rec)
	}
}

func assertSearchableIndexStats(t *testing.T, dir string) {
	t.Helper()
	st, err := nativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Orphans) != 0 || len(st.Dangling) != 0 || st.IndexEntries != 1 {
		t.Fatalf("stats = %+v, want 1 injected entry and no orphans/dangling", st)
	}
}

// TestNativeArchiveDirIsPerProject: two stores with different slugs archive into different dirs,
// so one project's archive never collides with another's.
func TestNativeArchiveDirIsPerProject(t *testing.T) {
	tempConfig(t)
	a := Dir(repoRoot(t, "repo-a"))
	b := Dir(repoRoot(t, "repo-b"))
	if nativeArchiveDir(a) == nativeArchiveDir(b) {
		t.Errorf("both projects archive into %s", nativeArchiveDir(a))
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
	if _, err := archiveNative(dir, []string{"link"}, true); err == nil || !fileExists(filepath.Join(dir, "link.md")) {
		t.Errorf("archiveNative(link) err = %v; want refusal with the symlink left in place", err)
	}
	st, err := nativeStats(dir)
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
	st, err := nativeStats(dir)
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
	archive := nativeArchiveDir(dir)
	first := mustRead(t, filepath.Join(dir, "stale-fact.md"))
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "stale-fact.md"), first+"\nrevised\n")
	res, err := archiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archived) != 1 || !strings.HasPrefix(res.Archived[0], "stale-fact@") || !strings.HasSuffix(res.Archived[0], ".md") {
		t.Fatalf("Archived = %v, want one stamped stale-fact@<utc>.md", res.Archived)
	}
	if got := mustRead(t, filepath.Join(archive, "stale-fact.md")); got != first {
		t.Errorf("the earlier revision was replaced: %q", got)
	}
	if got := mustRead(t, filepath.Join(archive, res.Archived[0])); got != first+"\nrevised\n" {
		t.Errorf("stamped copy = %q", got)
	}
	assertThirdRevisionDistinct(t, dir, first, res.Archived[0])
}

// assertThirdRevisionDistinct: a third revision in the same second gets a -2 suffix rather than replacing the stamped copy.
func assertThirdRevisionDistinct(t *testing.T, dir, first, stamped string) {
	t.Helper()
	archive := nativeArchiveDir(dir)
	write(t, filepath.Join(dir, "stale-fact.md"), first+"\nrevised again\n")
	third, err := archiveNative(dir, []string{"stale-fact"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Archived) != 1 || third.Archived[0] == stamped || !fileExists(filepath.Join(archive, stamped)) {
		t.Errorf("third archive = %v, want a distinct version beside %s", third.Archived, stamped)
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

// TestArchiveAndRestoreRefuseReservedAndBankNames: every reserved/typed name is refused with an
// error naming its kind, for both archiveNative and restoreNative.
func TestArchiveAndRestoreRefuseReservedAndBankNames(t *testing.T) {
	dir := nativeStore(t)
	archiveDir := nativeArchiveDir(dir)
	names := append(append([]string{}, BankFiles()...),
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
		if _, err := archiveNative(dir, []string{n}, true); err == nil {
			t.Errorf("archiveNative(%q) was allowed; want refusal naming the kind", n)
		}
		if _, err := restoreNative(dir, []string{n}, true); err == nil {
			t.Errorf("restoreNative(%q) was allowed; want refusal naming the kind", n)
		}
	}
}

// TestTopicPathsExcludeMetadataBankAndSymlink: a harness_kind:bank topic and a symlinked .md are
// not topics — topicPaths, nativeStats.TopicFiles and nativeDupes must all exclude them.
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

	st, err := nativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	// nativeStore seeds 3 topics; +real.md = 4. typed-notes.md and link.md excluded.
	if st.TopicFiles != 4 {
		t.Errorf("TopicFiles = %d, want 4 (metadata-bank file and symlink excluded)", st.TopicFiles)
	}

	clusters, err := nativeDupes(dir, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	var clustered []string
	for _, c := range clusters {
		clustered = append(append(clustered, c.Keep), c.Losers...)
	}
	assertExcludesNames(t, clustered, excluded, "nativeDupes")
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
// lines) is ignored by IndexEntries, never moved by splitNativeIndex, and never struck by archive.
func TestBankRegionNeverTouchedByIndexOpsOrArchive(t *testing.T) {
	dir := nativeStore(t)
	region := "<!-- bank:begin generated by claude-code memory index -->\n" +
		"- [Active](activeContext.md) — focus\n- [Progress](progress.md) — status\n" +
		"<!-- bank:end -->\n"
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+region)
	withRegion := mustRead(t, filepath.Join(dir, NativeIndexFile))
	start, end := bankRegionBounds(withRegion)
	if start < 0 || end < 0 {
		t.Fatalf("test setup: bank region not detected in:\n%s", withRegion)
	}
	regionBytes := withRegion[start:end]

	st, err := nativeStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.IndexEntries != 3 {
		t.Errorf("IndexEntries = %d, want 3 (bank region's 2 pointers excluded)", st.IndexEntries)
	}

	if _, err := splitNativeIndex(dir, 0); err != nil {
		t.Fatalf("splitNativeIndex: %v", err)
	}
	assertRegionUnchanged(t, filepath.Join(dir, NativeIndexFile), regionBytes, "splitNativeIndex")

	if _, err := archiveNative(dir, []string{"keep-me"}, true); err != nil {
		t.Fatalf("archiveNative: %v", err)
	}
	assertRegionUnchanged(t, filepath.Join(dir, NativeIndexFile), regionBytes, "archiveNative")
}

// assertRegionUnchanged fails if path's bank region is missing or no longer equals want; op names the caller for the message.
func assertRegionUnchanged(t *testing.T, path, want, op string) {
	t.Helper()
	got := mustRead(t, path)
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
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+"- [Active](activeContext.md) — focus\n")

	st, err := nativeStats(dir)
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
	before := mustRead(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), before+"- [A](a.md) — v1\n")

	if _, err := archiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 1: %v", err)
	}
	// A fresh a.md with other bytes while the v1 copy still sits in the archive: the collision case.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nversion two\n")
	idx := mustRead(t, filepath.Join(dir, NativeIndexFile))
	if !strings.Contains(idx, "a.md") {
		write(t, filepath.Join(dir, NativeIndexFile), idx+"- [A](a.md) — v2\n")
	}
	if _, err := archiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 2: %v", err)
	}
	archiveDir := nativeArchiveDir(dir)
	if got := archivedVersions(t, archiveDir, "a"); len(got) != 2 {
		t.Fatalf("archive entries = %v, want exactly a.md plus one a@<ts>.md", got)
	}

	assertArchiveDedupesAndRefusesDivergentRestore(t, dir, archiveDir)
}

func assertArchiveDedupesAndRefusesDivergentRestore(t *testing.T, dir, archiveDir string) {
	t.Helper()
	// Equal-content re-archive must dedupe, not add a third version.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nversion two\n")
	idx2 := mustRead(t, filepath.Join(dir, NativeIndexFile))
	write(t, filepath.Join(dir, NativeIndexFile), idx2+"- [A](a.md) — v2 again\n")
	if _, err := archiveNative(dir, []string{"a"}, true); err != nil {
		t.Fatalf("archive 3 (equal content): %v", err)
	}
	if got := archivedVersions(t, archiveDir, "a"); len(got) != 2 {
		t.Fatalf("equal-content re-archive changed the version count: %v", got)
	}

	// Restore onto a live a.md with different bytes must fail, leaving both intact.
	write(t, filepath.Join(dir, "a.md"), "---\nname: a\n---\n\nlive divergent content\n")
	if _, err := restoreNative(dir, []string{"a"}, true); err == nil {
		t.Error("restoreNative onto a divergent live file was allowed; want refusal")
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
	archiveDir := nativeArchiveDir(dir)
	write(t, filepath.Join(archiveDir, "activeContext.md"), "focus\n")
	if _, err := restoreNative(dir, []string{"activeContext.md"}, true); err == nil {
		t.Error("restoreNative(activeContext.md) was allowed because a file of that name sat in the archive")
	}
}

// TestArchiveNativeReturnsErrBankBusy: while another fd holds BankLockPath(), archiveNative
// must return ErrBankBusy (wrapped), and no "<lock>.lock" file is created anywhere.
func TestArchiveNativeReturnsErrBankBusy(t *testing.T) {
	shortBankWait(t)
	dir := nativeStore(t)
	if err := os.MkdirAll(filepath.Dir(BankLockPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	ran, err := paths.TryFileLock(BankLockPath(), 0, func() error {
		_, archErr := archiveNative(dir, []string{"stale-fact"}, true)
		if !errors.Is(archErr, ErrBankBusy) {
			t.Errorf("archiveNative under contention = %v, want errors.Is ErrBankBusy", archErr)
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

// TestMalformedBankRegionRefusesEveryIndexRewrite: a lone or doubled marker is a diagnostic, never a guess —
// split, archive, restore and stats all name MEMORY.md and leave every byte in place.
func TestMalformedBankRegionRefusesEveryIndexRewrite(t *testing.T) {
	dir := nativeStore(t)
	indexPath := filepath.Join(dir, NativeIndexFile)
	broken := mustRead(t, indexPath) + "<!-- bank:begin generated by claude-code memory index -->\n" +
		"- [Active](activeContext.md) — focus\n" // no end marker
	write(t, indexPath, broken)
	if _, err := archiveNative(dir, []string{"stale-fact"}, true); !errors.Is(err, errBankRegion) {
		t.Errorf("archiveNative err = %v, want errBankRegion", err)
	}
	if _, err := splitNativeIndex(dir, 0); !errors.Is(err, errBankRegion) {
		t.Errorf("splitNativeIndex err = %v, want errBankRegion", err)
	}
	if _, err := nativeStats(dir); !errors.Is(err, errBankRegion) {
		t.Errorf("nativeStats err = %v, want errBankRegion", err)
	}
	if got := mustRead(t, indexPath); got != broken {
		t.Errorf("a refused rewrite still changed %s:\n%s", NativeIndexFile, got)
	}
	if fileExists(filepath.Join(dir, NativeSearchableFile)) || fileExists(filepath.Join(nativeArchiveDir(dir), "stale-fact.md")) {
		t.Error("a refused rewrite still wrote SEARCHABLE.md or moved the topic")
	}
	write(t, filepath.Join(nativeArchiveDir(dir), "old.md"), "---\nname: old\n---\n\nbody\n")
	if _, err := restoreNative(dir, []string{"old"}, true); !errors.Is(err, errBankRegion) {
		t.Errorf("restoreNative err = %v, want errBankRegion", err)
	}
	if fileExists(filepath.Join(dir, "old.md")) {
		t.Error("a refused restore still landed the topic")
	}
}
