package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAccessLogPathMovesToStateAfterCutover: store dirs log under state/harness/memory/access, never inside memory/.
func TestAccessLogPathMovesToStateAfterCutover(t *testing.T) {
	migrateConfig(t)
	legacy := filepath.Join(t.TempDir(), "bank")
	if got := AccessLogPath(legacy); got != filepath.Join(legacy, AccessLogFile) {
		t.Fatalf("pre-cutover path = %s, want beside the bank", got)
	}
	cutover(t)
	for dir, want := range map[string]string{
		StoreDir():                           "global.log",
		filepath.Join(StoreDir(), ".claude"): ".claude.log",
		filepath.Join(StoreDir(), "repo-a"):  "repo-a.log",
	} {
		if got := AccessLogPath(dir); got != filepath.Join(AccessStateDir(), want) {
			t.Errorf("AccessLogPath(%s) = %s, want %s", dir, got, want)
		}
	}
	LogAccess(filepath.Join(StoreDir(), "repo-a"), time.Now(), "local:x.md", "read")
	if _, err := os.Stat(filepath.Join(StoreDir(), "repo-a", AccessLogFile)); !os.IsNotExist(err) {
		t.Errorf("access.log written inside the store: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(AccessStateDir(), "repo-a.log")); !strings.Contains(string(data), "local:x.md") {
		t.Errorf("state access log missing the entry: %q", data)
	}
}

// TestKnowledgeIsSearchedAndCataloged: docs/knowledge notes (not its README) are search hits and catalog rows.
func TestKnowledgeIsSearchedAndCataloged(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	write(t, filepath.Join(KnowledgeDir(), "README.md"), "# Knowledge\nzebracorn readme\n")
	write(t, filepath.Join(KnowledgeDir(), "zebra.md"), "# Zebra\nZebracorn invariant: probes never ship.\n")
	hits := SearchBanks(t.TempDir(), "zebracorn invariant probes", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "zebra.md" {
		t.Fatalf("knowledge not ranked first: %+v", hits)
	}
	for _, h := range hits {
		if h.File == "README.md" {
			t.Errorf("the tier README was indexed as knowledge")
		}
	}
	c, err := BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, "## Knowledge") || !strings.Contains(c, "zebra.md — Zebracorn invariant") || strings.Contains(c, "README.md —") {
		t.Errorf("catalog knowledge section wrong (err=%v):\n%s", err, c)
	}
}

// TestUnifiedSurfaceFallsBackWhenNativeUnlinked: only a missing/blocked native link adds the store's MEMORY.md.
func TestUnifiedSurfaceFallsBackWhenNativeUnlinked(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	repo := canonTemp(t, t.TempDir())
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Skip("git unavailable")
	}
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, NativeIndexFile), "- [Topic](topic.md) — fallback marker\n")
	if s := unifiedSurface(repo); !strings.Contains(s, "native link missing") || !strings.Contains(s, "fallback marker") {
		t.Fatalf("unlinked surface lacks the MEMORY.md fallback:\n%s", s)
	}
	if _, ok := LinkNativeFor(repo); !ok {
		t.Fatal("LinkNativeFor failed")
	}
	if s := unifiedSurface(repo); strings.Contains(s, "native link missing") {
		t.Errorf("linked surface still carries the fallback:\n%s", s)
	}
}
