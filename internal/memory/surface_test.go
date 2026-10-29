package memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The catalog leaves the surface (one pointer stays); the Parked section leaves the injected surface (with a count), the other sections stay, and recall can still find it.
func TestUnifiedSurfaceFocusOnly(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(StoreDir(), CatalogFile), "# Memory catalog\n\n- proj — hook · last never -> proj/MEMORY.md\n")
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

	s := unifiedSurface(repo)
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
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n")
	for i := range 200 {
		fmt.Fprintf(&cat, "- repo-%03d — hook · last never -> repo-%03d/MEMORY.md\n", i, i)
	}
	write(t, filepath.Join(StoreDir(), CatalogFile), cat.String())
	write(t, filepath.Join(dir, "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- local focus\n")
	write(t, filepath.Join(HomeBankDir(), "activeContext.md"), "# Active Context\n\n## Current Focus\n\n- home essential\n")

	s := unifiedSurface(repo)
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
