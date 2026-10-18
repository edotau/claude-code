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
	for _, l := range strings.Split(string(data), "\n") {
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
