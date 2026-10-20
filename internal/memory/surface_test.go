package memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The Parked section leaves the injected surface (with a count), the other sections stay, and recall can still find it.
func TestSessionSurfaceDropsParked(t *testing.T) {
	ac := "# Active Context\n\n## Current Focus\n\n- shipping the router\n\n## Parked\n\n- home network gateway firmware\n- interview prep notes\n\n## Blockers\n\n- none\n"
	got := dropParked(ac)
	for _, want := range []string{"shipping the router", "## Blockers", "2 parked item(s) omitted"} {
		if !strings.Contains(got, want) {
			t.Errorf("surface lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "gateway firmware") || strings.Contains(got, "## Parked") {
		t.Errorf("parked content leaked into the surface:\n%s", got)
	}
	if in := "# A\n\n## Current Focus\n\n- x\n"; dropParked(in) != in {
		t.Error("a file without Parked must pass through unchanged")
	}
}

// TestUnifiedSurfaceCatalogAndFocusOnly: the post-cutover surface carries the catalog plus only
// activeContext's Current Focus/Blockers/Next Steps and progress's In Progress — nothing else — and
// stays within TotalCapChars.
func TestUnifiedSurfaceCatalogAndFocusOnly(t *testing.T) {
	migrateConfig1(t)
	cutover(t)
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
	for _, want := range []string{"MEMORY CATALOG", "## Current Focus", "shipping the router",
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

// TestUnifiedSurfaceCatalogTruncatesAtRowBoundary: a huge catalog is cut at a whole-line boundary, never
// mid-row, with a pointer back to the full file.
func TestUnifiedSurfaceCatalogTruncatesAtRowBoundary(t *testing.T) {
	migrateConfig1(t)
	cutover(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	var cat strings.Builder
	cat.WriteString("# Memory catalog\n\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&cat, "- repo-%03d — hook · last never -> repo-%03d/MEMORY.md\n", i, i)
	}
	write(t, filepath.Join(StoreDir(), CatalogFile), cat.String())

	s := unifiedSurface(repo)
	if !strings.Contains(s, "catalog truncated; read "+CatalogFile+" in the store") {
		t.Errorf("huge catalog not truncated:\n%s", s[:min(len(s), 400)])
	}
	if strings.Contains(s, "- repo-199") {
		t.Error("catalog truncation did not actually cut the tail")
	}
	for l := range strings.SplitSeq(s, "\n") {
		if strings.HasPrefix(l, "- repo-") && !strings.Contains(l, "/MEMORY.md") {
			t.Errorf("a catalog row was cut mid-line: %q", l)
		}
	}
}

// TestSectionBodyEdgeCases: case-insensitive heading match, an absent heading, an empty section, and the
// last section in the file all resolve correctly.
func TestSectionBodyEdgeCases(t *testing.T) {
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
