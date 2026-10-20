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
	migrateConfig(t)
	cutover(t)
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

// TestAssignIDsDuplicateContentGetsDistinctIDs: two blocks with identical header text must not collide on one ID.
func TestAssignIDsDuplicateContentGetsDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "decisionLog.md"), "## Fix the bug\nbody one\n\n## Fix the bug\nbody two\n")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if n, err := assignIDs(dir, now); err != nil || n != 2 {
		t.Fatalf("assignIDs = %d, %v; want 2", n, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "decisionLog.md"))
	var ids []string
	for l := range strings.SplitSeq(string(data), "\n") {
		if id := BlockID(l); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("duplicate content got duplicate ids: %v", ids)
	}
}

// TestAssignIDsSecondPassLeavesExistingIDsByteIdentical: a stamped block must never be re-stamped, even byte-wise.
func TestAssignIDsSecondPassLeavesExistingIDsByteIdentical(t *testing.T) {
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

// TestDigestInputIsRawArchiveOnly: the digest reads every raw archive file but never its own output, and refuses past the cap.
func TestDigestInputIsRawArchiveOnly(t *testing.T) {
	migrateConfig(t)
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
