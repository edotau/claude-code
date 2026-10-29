package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBlockIDsPersistAcrossEditsAndArchive: IDs are stamped once, survive a title edit, and `show` finds a block in an archive.
func TestBlockIDsPersistAcrossEditsAndArchive(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	dir := filepath.Join(StoreDir(), "repo")
	log := filepath.Join(dir, "decisionLog.md")
	write(t, log, "# Decision Log\n\n## 2026-10-18 — First\nbody one\n\n## 2026-10-17 — Second\nbody two\n")
	now := time.Now()
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("first stamp = %d, %v; want 2", n, err)
	}
	if n, _ := assignIDs(dir, now.Add(time.Second)); n != 0 {
		t.Errorf("second stamp re-stamped %d blocks; IDs must be assigned once", n)
	}
	data, _ := os.ReadFile(log)
	var id string
	for l := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(l, "## 2026-10-18") {
			id = BlockID(l)
		}
	}
	if len(id) != 8 {
		t.Fatalf("no id on the first block:\n%s", data)
	}
	write(t, log, strings.Replace(string(data), "— First", "— First, retitled", 1))
	if _, block, err := ShowBlock(id); err != nil || !strings.Contains(block, "retitled") || !strings.Contains(block, "body one") {
		t.Errorf("show after a title edit = %q, %v", block, err)
	}
	moved, _ := os.ReadFile(log)
	write(t, filepath.Join(dir, "archive", "decisionLog-2026-10.md"), string(moved))
	write(t, log, "# Decision Log\n")
	if path, _, err := ShowBlock(id); err != nil || !strings.Contains(path, "archive") {
		t.Errorf("show after archiving = %s, %v; want the archive file", path, err)
	}
	if _, _, err := ShowBlock("deadbeef"); err != ErrNoBlock {
		t.Errorf("unknown id err = %v, want ErrNoBlock", err)
	}
	if e := entriesWithPrefix(filepath.Join(dir, "archive", "decisionLog-2026-10.md"), "## "); len(e) == 0 || !strings.HasSuffix(e[0], "["+id+"]") || strings.Contains(e[0], "<!--") {
		t.Errorf("index line must show [id] without the raw marker: %v", e)
	}
}

// TestAssignIDsSecondPassLeavesExistingIDsByteIdentical: a stamped block must never be re-stamped, even byte-wise.
func TestAssignIDsSecondPassLeavesExistingIDsByteIdentical(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "## First\nbody one\n\n## Second\nbody two\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("first stamp = %d, %v; want 2", n, err)
	}
	before, _ := os.ReadFile(path)
	if n, err := assignIDs(dir, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("second stamp = %d, %v; want 0 (already stamped)", n, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("second pass changed stamped content:\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs: three byte-identical block headers must get three distinct IDs.
func TestAssignIDsThreeIdenticalBlocksGetThreeDistinctIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "## Fix the bug\nbody one\n\n## Fix the bug\nbody two\n\n## Fix the bug\nbody three\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 3 {
		t.Fatalf("assignIDs = %d, %v; want 3", n, err)
	}
	data, _ := os.ReadFile(path)
	seen := map[string]bool{}
	for l := range strings.SplitSeq(string(data), "\n") {
		if id := BlockID(l); id != "" {
			if seen[id] {
				t.Fatalf("duplicate id %q across identical blocks", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct ids, want 3: %v", len(seen), seen)
	}
}

// TestAssignIDsSameTextDifferentFilesGetDistinctIDs: the same block text stamped in two different typed files must not collide.
func TestAssignIDsSameTextDifferentFilesGetDistinctIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "decisionLog.md"), "## Fix the bug\nbody\n")
	write(t, filepath.Join(dir, "sessionHistory.md"), "## Fix the bug\nbody\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("assignIDs = %d, %v; want 2", n, err)
	}
	var ids []string
	for _, name := range []string{"decisionLog.md", "sessionHistory.md"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		for l := range strings.SplitSeq(string(data), "\n") {
			if id := BlockID(l); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("same text in different files got duplicate ids: %v", ids)
	}
}

// TestContentRefStableAcrossRunsAndResolvesViaShow: an id-less block's ContentRef is identical
// across two independent SearchBanks calls and resolves through ShowBlock; a block carrying a real
// persisted id keeps that id rather than getting a derived ref.
func TestContentRefStableAcrossRunsAndResolvesViaShow(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "widget")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n- never stamped focus item about widgets\n")
	write(t, filepath.Join(dir, "decisionLog.md"), "# Decisions\n\n## widget policy <!-- id:cafebabe -->\nkeep it simple\n")

	first := SearchBanks(repo, "never stamped focus widgets", 0)
	second := SearchBanks(repo, "never stamped focus widgets", 0)
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("fixture produced no stable hit set: first=%#v second=%#v", first, second)
	}
	var ref string
	for i, h := range first {
		if h.Block != second[i].Block || h.ID != second[i].ID {
			t.Fatalf("hit %d unstable across runs: %#v vs %#v", i, h, second[i])
		}
		if h.File == "activeContext.md" {
			ref = h.ID
		}
	}
	if ref == "" || !strings.HasPrefix(ref, contentRefPrefix) {
		t.Fatalf("unstamped hit missing a content ref: %#v", first)
	}
	if _, block, err := ShowBlock(ref); err != nil || !strings.Contains(block, "widgets") {
		t.Fatalf("ShowBlock(%s) = %q, %v; want the unstamped block", ref, block, err)
	}

	stamped := SearchBanks(repo, "widget policy keep simple", 0)
	var sawRealID bool
	for _, h := range stamped {
		if h.File == "decisionLog.md" {
			if h.ID != "cafebabe" {
				t.Errorf("a block with a persisted id got %q instead of its own marker", h.ID)
			}
			sawRealID = true
		}
	}
	if !sawRealID {
		t.Fatal("decisionLog hit missing from the stamped-block search")
	}
}

// TestDigestInputIsRawArchiveOnly: the digest reads every raw archive file but never its own output, and refuses past the cap.
func TestDigestInputIsRawArchiveOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "store")
	write(t, filepath.Join(ArchiveDir(dir), "decisionLog-2026-09.md"), "## 2026-09-01 — old\nraw body\n")
	write(t, filepath.Join(ArchiveDir(dir), DigestFile), "# Archive digest\n- previous summary\n")
	in, used, err := DigestInput(dir)
	if err != nil || len(used) != 1 || !strings.Contains(in, "raw body") || strings.Contains(in, "previous summary") {
		t.Fatalf("DigestInput = %q %v %v; want the raw file only", in, used, err)
	}
	write(t, filepath.Join(ArchiveDir(dir), "sessionHistory-2026-09.md"), strings.Repeat("x", digestMaxInput))
	if _, _, err := DigestInput(dir); err == nil || !strings.Contains(err.Error(), "needs-scope") {
		t.Errorf("over-cap archive err = %v, want needs-scope", err)
	}
}

// A marker the formatter wrapped onto a later line counts: settling twice adds nothing.
func TestAssignIDsSeesMarkerOnLaterLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conventions.md")
	body := "# C\n\n- long convention wrapped\n  onto two lines <!-- id:aaaa0001 -->\n\n- plain rule\n"
	write(t, path, body)
	n, err := assignIDs(dir, time.Now())
	if err != nil || n != 1 { // only "plain rule" is unmarked
		t.Fatalf("first settle stamped %d, err %v", n, err)
	}
	once, _ := os.ReadFile(path)
	if !strings.Contains(string(once), "- long convention wrapped <!-- id:aaaa0001 -->\n  onto two lines\n") {
		t.Errorf("a wrapped marker must move onto the block's first line:\n%s", once)
	}
	if n, _ = assignIDs(dir, time.Now().Add(time.Second)); n != 0 {
		t.Errorf("second settle stamped %d", n)
	}
	twice, _ := os.ReadFile(path)
	if string(once) != string(twice) || strings.Count(string(twice), "<!-- id:") != 2 {
		t.Errorf("second settle changed the file:\n%s\n---\n%s", once, twice)
	}
}

// Stacked markers collapse to the oldest (last) one, idempotently.
func TestAssignIDsCollapsesStackedMarkersToOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conventions.md")
	write(t, path, "# C\n\n- a rule that wraps\n  <!-- id:00000005 -->\n  <!-- id:00000004 -->\n  <!-- id:00000003 -->\n  <!-- id:00000002 -->\n  <!-- id:00000001 -->\n\n- next\n")
	if _, err := assignIDs(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Count(s, "<!-- id:00000001 -->") != 1 || strings.Count(s, "<!-- id:") != 2 { // the other is "next"'s fresh id
		t.Fatalf("want only the oldest marker kept on the wrapped block:\n%s", s)
	}
	if _, err := assignIDs(dir, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != s {
		t.Errorf("collapse is not idempotent:\n%s\n---\n%s", s, again)
	}
	if first := strings.Split(s, "\n")[2]; !strings.HasSuffix(first, "wraps <!-- id:00000001 -->") {
		t.Errorf("the retained marker must end on the block's first line, got %q", first)
	}
}

// A fenced example marker is not the block's id: settle keeps the real one and never adopts the example.
func TestAssignIDsIgnoresFencedMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisionLog.md")
	write(t, path, "# D\n\n## 2026-10-01 — real heading <!-- id:aaaa0001 -->\n\nexample:\n\n```md\n## x <!-- id:bbbb0002 -->\n```\n")
	if _, err := assignIDs(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if !strings.Contains(s, "real heading <!-- id:aaaa0001 -->") || strings.Count(s, "<!-- id:bbbb0002 -->") != 1 {
		t.Errorf("real id lost or example touched:\n%s", s)
	}
	if id := blockMarkerID(strings.Join(strings.Split(s, "\n")[2:], "\n")); id != "aaaa0001" {
		t.Errorf("canonical id = %q, want the unfenced one", id)
	}
	// an unmarked block whose only marker is a fenced example still gets its own id
	write(t, path, "# D\n\n## 2026-10-02 — bare\n\n```md\n## x <!-- id:cccc0003 -->\n```\n")
	if n, _ := assignIDs(dir, time.Now()); n != 1 {
		t.Errorf("bare block with a fenced example must be stamped, stamped %d", n)
	}
}

// Content refs of native topics and knowledge notes (indexed whole) expand through ShowBlock.
func TestShowBlockExpandsNativeAndKnowledgeRefs(t *testing.T) {
	isolatedConfig1(t)
	note := filepath.Join(HomeBankDir(), "knowledge", "wires.md")
	write(t, note, "---\ndescription: d\n---\n# Wires\nknowledge body zebra\n")
	var got SearchHit
	for _, h := range corpusBlocks(t.TempDir()) {
		if h.Bank == "knowledge" {
			got = h
		}
	}
	if got.Block == "" {
		t.Fatal("fixture: knowledge note not indexed")
	}
	if _, block, err := ShowBlock(EntryFromHit(got).ID); err != nil || !strings.Contains(block, "knowledge body zebra") {
		t.Errorf("knowledge ref does not expand: %v %q", err, block)
	}
}
