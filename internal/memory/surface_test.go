package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
)

// The catalog leaves the surface (one pointer stays); the Parked section leaves the injected surface (with a count), the other sections stay, and recall can still find it.
func TestUnifiedSurfaceFocusOnly(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := memstore.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(memstore.StoreDir(), memstore.CatalogFile), "# Memory catalog\n\n- proj — hook · last never -> proj/MEMORY.md\n")
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n"+
		"## Current Focus\n\n- shipping the router\n\n"+
		"## Open Questions\n\n- should not appear\n\n"+
		"## Blockers\n\n- none\n\n"+
		"## Next Steps\n\n- ship it\n\n"+
		"## Parked\n\n- should not appear either\n")
	write(t, filepath.Join(dir, "progress.md"), "# Progress\n\n"+
		"## What Works\n\n- should not appear\n\n"+
		"## In Progress\n\n- the router\n\n"+
		"## What's Next\n\n- should not appear\n")

	s := SessionSurface(repo)
	for _, want := range []string{"other repos:", "## Current Focus", "shipping the router",
		"## Blockers", "## Next Steps", "ship it", "## In Progress", "the router"} {
		if !strings.Contains(s, want) {
			t.Errorf("surface missing %q:\n%s", want, s)
		}
	}
	for _, absent := range []string{"Open Questions", "should not appear", "What Works", "What's Next"} {
		if strings.Contains(s, absent) {
			t.Errorf("surface carried an out-of-scope section (%q):\n%s", absent, s)
		}
	}
	if n := len([]rune(s)); n > TotalCapChars {
		t.Errorf("unified surface %d chars > cap %d", n, TotalCapChars)
	}
}

// TestUnifiedSurfaceCarriesNoCatalogRows: a 200-repo catalog adds one pointer line and no rows, and the home
// bank's focus rides along with this repo's.
func TestUnifiedSurfaceCarriesNoCatalogRows(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := memstore.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n")
	for i := range 200 {
		fmt.Fprintf(&cat, "- repo-%03d — hook · last never -> repo-%03d/MEMORY.md\n", i, i)
	}
	write(t, filepath.Join(memstore.StoreDir(), memstore.CatalogFile), cat.String())
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- local focus\n")
	write(t, filepath.Join(memstore.HomeBankDir(), "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- home essential\n")

	s := SessionSurface(repo)
	if strings.Contains(s, "repo-000") || strings.Contains(s, "repo-199") || strings.Contains(s, "MEMORY CATALOG") {
		t.Errorf("catalog rows leaked into the surface:\n%s", s)
	}
	for _, want := range []string{"other repos:", "local focus", "home essential"} {
		if !strings.Contains(s, want) {
			t.Errorf("surface missing %q:\n%s", want, s)
		}
	}
	if len(s) > 1200 {
		t.Errorf("slim surface is %d bytes, want well under the old 8.5 KB cap", len(s))
	}
}

// TestSectionBodyEdgeCases: case-insensitive heading match, an absent heading, an empty section, and the
// last section in the file all resolve correctly.
func TestSectionBodyEdgeCases(t *testing.T) {
	t.Parallel()
	text := "# Active\n\n## Current Focus\n\n- a\n- b\n\n## Blockers\n\n- none\n"
	if got := sectionBody(text, "Current Focus"); got != "- a\n- b" {
		t.Errorf("sectionBody = %q", got)
	}
	if got := sectionBody(text, "current focus"); got != "- a\n- b" {
		t.Errorf("sectionBody must be case-insensitive, got %q", got)
	}
	if got := sectionBody(text, "Next Steps"); got != "" {
		t.Errorf("an absent heading must yield \"\", got %q", got)
	}
	if got := sectionBody("# Active\n\n## Empty\n\n## Blockers\n\n- x\n", "Empty"); got != "" {
		t.Errorf("an empty section must yield \"\", got %q", got)
	}
	if got := sectionBody(text, "Blockers"); got != "- none" {
		t.Errorf("the last section in the file must still be captured, got %q", got)
	}
}

// Correctness fix (mdsmith AST headings, md-spec.md): a "## " line inside a fenced code block is no
// longer mistaken for the next heading, so the fence's full contents stay inside the section.
func TestSectionBodyRespectsFence(t *testing.T) {
	t.Parallel()
	text := "# Active\n\n## Current Focus\n\n```\n## not a heading\n```\n- a\n\n## Blockers\n\n- none\n"
	want := "```\n## not a heading\n```\n- a"
	if got := sectionBody(text, "Current Focus"); got != want {
		t.Errorf("sectionBody = %q, want %q (fence-aware heading scan must keep the fenced \"## \" line in-section)", got, want)
	}
}

// TestAccessLogPathMovesToStateAfterCutover: store dirs log under state/harness/memory/access, never inside memory/.
func TestAccessLogPathMovesToStateAfterCutover(t *testing.T) {
	isolatedConfig(t)
	legacy := filepath.Join(t.TempDir(), "bank")
	if got := recall.AccessLogPath(legacy); got != filepath.Join(legacy, recall.AccessLogFile) {
		t.Fatalf("pre-cutover path = %s, want beside the bank", got)
	}
	initStore(t)
	for dir, want := range map[string]string{
		memstore.StoreDir():                           "home.log",
		filepath.Join(memstore.StoreDir(), ".claude"): ".claude.log",
		filepath.Join(memstore.StoreDir(), "repo-a"):  "repo-a.log",
	} {
		if got := recall.AccessLogPath(dir); got != filepath.Join(memstore.AccessStateDir(), want) {
			t.Errorf("recall.AccessLogPath(%s) = %s, want %s", dir, got, want)
		}
	}
	recall.LogAccess(filepath.Join(memstore.StoreDir(), "repo-a"), time.Now(), "aa11bb22", "read")
	if _, err := os.Stat(filepath.Join(memstore.StoreDir(), "repo-a", recall.AccessLogFile)); !os.IsNotExist(err) {
		t.Errorf("access.log written inside the store: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(memstore.AccessStateDir(), "repo-a.log")); !strings.Contains(string(data), "aa11bb22") {
		t.Errorf("state access log missing the entry: %q", data)
	}
}

// TestKnowledgeIsSearchedAndCataloged: memory/<bank>/knowledge notes (not its README) are search hits and catalog rows.
func TestKnowledgeIsSearchedAndCataloged(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	write(t, filepath.Join(memstore.KnowledgeDir(memstore.HomeBankDir()), "README.md"), "# Knowledge\nzebracorn readme\n")
	write(t, filepath.Join(memstore.KnowledgeDir(memstore.HomeBankDir()), "zebra.md"), "# Zebra\nZebracorn invariant: probes never ship.\n")
	hits := recall.SearchBanks(t.TempDir(), "zebracorn invariant probes", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "zebra.md" {
		t.Fatalf("knowledge not ranked first: %+v", hits)
	}
	for _, h := range hits {
		if h.File == "README.md" {
			t.Errorf("the tier README was indexed as knowledge")
		}
	}
	c, err := bank.BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, "## Knowledge") || !strings.Contains(c, "home/knowledge/zebra.md — Zebracorn invariant") || strings.Contains(c, "README.md —") {
		t.Errorf("catalog knowledge section wrong (err=%v):\n%s", err, c)
	}
}

// TestRepoBankKnowledgeIsCatalogedAndSearched: a repo bank's knowledge/*.md shows under its own key in the
// catalog and is reachable by search, same as the home-bank tier.
func TestRepoBankKnowledgeIsCatalogedAndSearched(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := gitRepo(t, t.TempDir(), "narwhal")
	dir, err := memstore.EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(memstore.KnowledgeDir(memstore.Dir(root)), "README.md"), "# Knowledge\nnarwhal readme\n")
	write(t, filepath.Join(memstore.KnowledgeDir(memstore.Dir(root)), "narwhal.md"), "# Narwhal\nNarwhalcorn invariant: tusks never ship.\n")
	hits := recall.SearchBanks(root, "narwhalcorn invariant tusks", 5)
	if len(hits) == 0 || hits[0].Bank != "knowledge" || hits[0].File != "narwhal.md" {
		t.Fatalf("repo bank knowledge not ranked first: %+v", hits)
	}
	key := filepath.Base(dir)
	c, err := bank.BuildCatalog(time.Now())
	if err != nil || !strings.Contains(c, key+"/knowledge/narwhal.md — Narwhalcorn invariant") || strings.Contains(c, key+"/README.md —") {
		t.Errorf("catalog missing repo bank knowledge row (err=%v):\n%s", err, c)
	}
}

// TestLegacyDocsKnowledgeIgnored: a leftover <ConfigDir>/docs/knowledge tree is no longer searched or cataloged.
func TestLegacyDocsKnowledgeIgnored(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	initStore(t)
	write(t, filepath.Join(cfg, "docs", "knowledge", "yak.md"), "# Yak\nYakcorn invariant: wool never ships.\n")
	if hits := recall.SearchBanks(t.TempDir(), "yakcorn invariant wool", 5); len(hits) != 0 {
		t.Errorf("legacy docs/knowledge note was searched: %+v", hits)
	}
	c, err := bank.BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c, "yak.md") {
		t.Errorf("legacy docs/knowledge note leaked into the catalog:\n%s", c)
	}
}

// TestUnifiedSurfaceFallsBackWhenNativeUnlinked: only a missing/blocked native link adds the store's MEMORY.md.
func TestUnifiedSurfaceFallsBackWhenNativeUnlinked(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	repo := canonTemp(t, t.TempDir())
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Skip("git unavailable")
	}
	dir, err := memstore.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, bank.NativeIndexFile), "- [Topic](topic.md) — fallback marker\n")
	if s := SessionSurface(repo); !strings.Contains(s, "native link missing") || !strings.Contains(s, "fallback marker") {
		t.Fatalf("unlinked surface lacks the MEMORY.md fallback:\n%s", s)
	}
	if _, ok := memstore.LinkNativeFor(repo); !ok {
		t.Fatal("LinkNativeFor failed")
	}
	if s := SessionSurface(repo); strings.Contains(s, "native link missing") {
		t.Errorf("linked surface still carries the fallback:\n%s", s)
	}
}

func TestSectionBodyBareHeadingDoesNotPanic(t *testing.T) {
	if got := sectionBody("## \nfoo\n## Current Focus\nbar\n", "Current Focus"); got != "bar" {
		t.Errorf("sectionBody = %q, want %q", got, "bar")
	}
}
