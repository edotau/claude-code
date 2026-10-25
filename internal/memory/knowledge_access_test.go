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
		StoreDir():                           "home.log",
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

// TestKnowledgeIsSearchedAndCataloged: memory/<bank>/knowledge notes (not its README) are search hits and catalog rows.
func TestKnowledgeIsSearchedAndCataloged(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	write(t, filepath.Join(KnowledgeDir(HomeBankDir()), "README.md"), "# Knowledge\nzebracorn readme\n")
	write(t, filepath.Join(KnowledgeDir(HomeBankDir()), "zebra.md"), "# Zebra\nZebracorn invariant: probes never ship.\n")
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
	if err != nil || !strings.Contains(c, "## Knowledge") || !strings.Contains(c, "home/knowledge/zebra.md — Zebracorn invariant") || strings.Contains(c, "README.md —") {
		t.Errorf("catalog knowledge section wrong (err=%v):\n%s", err, c)
	}
}

// TestRepoBankKnowledgeIsCatalogedAndSearched: a repo bank's knowledge/*.md shows under its own key in the
// catalog and is reachable by search, same as the home-bank tier.
func TestRepoBankKnowledgeIsCatalogedAndSearched(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	root := gitRepo(t, t.TempDir(), "narwhal")
	dir, err := EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(KnowledgeDir(Dir(root)), "README.md"), "# Knowledge\nnarwhal readme\n")
	write(t, filepath.Join(KnowledgeDir(Dir(root)), "narwhal.md"), "# Narwhal\nNarwhalcorn invariant: tusks never ship.\n")
	hits := SearchBanks(root, "narwhalcorn invariant tusks", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "narwhal.md" {
		t.Fatalf("repo bank knowledge not ranked first: %+v", hits)
	}
	key := filepath.Base(dir)
	c, err := BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, key+"/knowledge/narwhal.md — Narwhalcorn invariant") || strings.Contains(c, key+"/README.md —") {
		t.Errorf("catalog missing repo bank knowledge row (err=%v):\n%s", err, c)
	}
}

// TestLegacyHomeKnowledgeFoundAndCataloged: a leftover <ConfigDir>/docs/knowledge/*.md note is still a
// search hit and a catalog row tagged "(legacy)", alongside the native note (untagged), with no duplicates
// and the legacy README excluded same as the native one.
func TestLegacyHomeKnowledgeFoundAndCataloged(t *testing.T) {
	cfg, _ := migrateConfig(t)
	cutover(t)
	write(t, filepath.Join(KnowledgeDir(HomeBankDir()), "zebra.md"), "# Zebra\nZebracorn invariant: probes never ship.\n")
	write(t, filepath.Join(cfg, "docs", "knowledge", "README.md"), "# Knowledge\nlegacy readme\n")
	write(t, filepath.Join(cfg, "docs", "knowledge", "yak.md"), "# Yak\nYakcorn invariant: wool never ships.\n")

	hits := SearchBanks(t.TempDir(), "yakcorn invariant wool", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "yak.md" {
		t.Fatalf("legacy knowledge not ranked first: %+v", hits)
	}
	for _, h := range hits {
		if h.File == "README.md" {
			t.Errorf("the legacy tier README was indexed as knowledge")
		}
	}

	c, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c, "home/knowledge/yak.md — Yakcorn invariant: wool never ships. (legacy)") {
		t.Errorf("catalog missing the legacy-tagged row:\n%s", c)
	}
	if !strings.Contains(c, "home/knowledge/zebra.md — Zebracorn invariant: probes never ship.\n") {
		t.Errorf("catalog native row got legacy-tagged or is missing:\n%s", c)
	}
	if strings.Contains(c, "README.md —") {
		t.Errorf("a README leaked into the catalog (native or legacy):\n%s", c)
	}
	if strings.Count(c, "yak.md") != 1 || strings.Count(c, "zebra.md") != 1 {
		t.Errorf("knowledge rows duplicated:\n%s", c)
	}
}

// TestLegacyRepoKnowledgeMapsToThatRepo: <ConfigDir>/docs/knowledge/<repo-key>/*.md is found and cataloged
// under that repo's own key, not under home, and the home tier stays untouched by it.
func TestLegacyRepoKnowledgeMapsToThatRepo(t *testing.T) {
	cfg, _ := migrateConfig(t)
	cutover(t)
	root := gitRepo(t, t.TempDir(), "narwhal")
	dir, err := EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Base(dir)
	write(t, filepath.Join(cfg, "docs", "knowledge", key, "README.md"), "# Knowledge\nnarwhal legacy readme\n")
	write(t, filepath.Join(cfg, "docs", "knowledge", key, "narwhal.md"), "# Narwhal\nNarwhalcorn invariant: tusks never ship.\n")

	hits := SearchBanks(root, "narwhalcorn invariant tusks", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "narwhal.md" {
		t.Fatalf("legacy repo knowledge not ranked first: %+v", hits)
	}

	c, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c, key+"/knowledge/narwhal.md — Narwhalcorn invariant: tusks never ship. (legacy)") {
		t.Errorf("catalog missing the repo's legacy row under its own key:\n%s", c)
	}
	if strings.Contains(c, "home/knowledge/narwhal.md") {
		t.Errorf("repo-keyed legacy note leaked into the home tier:\n%s", c)
	}
}

// TestLegacyKnowledgeNoHomeSubdirForHomeRoot: a stray <ConfigDir>/docs/knowledge/home/*.md (a subdir
// literally named "home") is never pulled in for the home root — only the top-level glob is, and the
// subdir form is reserved for a real repo key.
func TestLegacyKnowledgeNoHomeSubdirForHomeRoot(t *testing.T) {
	cfg, _ := migrateConfig(t)
	cutover(t)
	write(t, filepath.Join(cfg, "docs", "knowledge", "home", "decoy.md"), "# Decoy\nDecoycorn invariant: should not surface.\n")

	hits := SearchBanks(t.TempDir(), "decoycorn invariant", 5)
	if len(hits) != 0 {
		t.Errorf("docs/knowledge/home/ subdir was pulled into the home root: %+v", hits)
	}
	c, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c, "decoy.md") {
		t.Errorf("docs/knowledge/home/ subdir leaked into the catalog:\n%s", c)
	}
}

// TestLegacyKnowledgeSurfacesOnlyPostCutover: the legacy tier is a compat read gated the same as the rest
// of the knowledge corpus — search finds it after CutOver(), never before.
func TestLegacyKnowledgeSurfacesOnlyPostCutover(t *testing.T) {
	cfg, _ := migrateConfig(t)
	write(t, filepath.Join(cfg, "docs", "knowledge", "yak.md"), "# Yak\nYakcorn invariant: wool never ships.\n")

	if hits := SearchBanks(t.TempDir(), "yakcorn invariant wool", 5); len(hits) != 0 {
		t.Fatalf("pre-cutover search found the legacy note: %+v", hits)
	}

	cutover(t)
	hits := SearchBanks(t.TempDir(), "yakcorn invariant wool", 5)
	if len(hits) == 0 || hits[0].File != "yak.md" {
		t.Fatalf("post-cutover search missed the legacy note: %+v", hits)
	}
}

// TestLegacyKnowledgeFilesUntouched: reading the legacy tier (search + catalog) is a compat read only —
// it never writes, moves or deletes anything under docs/knowledge/.
func TestLegacyKnowledgeFilesUntouched(t *testing.T) {
	cfg, _ := migrateConfig(t)
	cutover(t)
	path := filepath.Join(cfg, "docs", "knowledge", "yak.md")
	body := "# Yak\nYakcorn invariant: wool never ships.\n"
	write(t, path, body)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	SearchBanks(t.TempDir(), "yakcorn invariant wool", 5)
	if _, err := BuildCatalog(time.Now()); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("legacy file disappeared: %v", err)
	}
	if before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Errorf("legacy file was modified: before=%+v after=%+v", before, after)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != body {
		t.Errorf("legacy file content changed: %q", data)
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
