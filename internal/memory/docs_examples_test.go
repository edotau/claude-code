package memory

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// update rewrites the docs/templates/examples/memory/* golden files this test guards (same convention
// as internal/settings's TestRenderGolden): `go test ./internal/memory -run TestDocsTemplateExamples -update`.
var update = flag.Bool("update", false, "rewrite docs/templates/examples/memory golden files")

// docsExamplesDir is this repo's docs/templates/examples/memory, reached from internal/memory.
func docsExamplesDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "docs", "templates", "examples", "memory")
}

// checkExample writes got to name under docs/templates/examples/memory when -update is set, else asserts
// the checked-in file already equals got.
func checkExample(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(docsExamplesDir(t), name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s drifted from docs/templates/examples/memory/%s (go test ./internal/memory -run TestDocsTemplateExamples -update):\n%s", name, name, got)
	}
}

// TestDocsTemplateExamplesCatalog renders docs/templates/examples/memory/index.md from two fixture repo
// stores with fixed fake origins (no real paths) and a handful of global conventions.
func TestDocsTemplateExamplesCatalog(t *testing.T) {
	isolatedConfig1(t)
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	mtime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) // noon UTC: the "last" column's calendar date holds in any local TZ

	for _, r := range []struct {
		key, origin, context string
	}{
		{"alpha-example", "/home/user/code/alpha-example", "alpha-example: a sample API service, routes deploys over ssh\n"},
		{"beta-example", "/home/user/code/beta-example", "beta-example: a sample CLI tool, ships as a single binary\n"},
	} {
		dir := filepath.Join(StoreDir(), r.key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: r.key, Origin: r.origin}); err != nil {
			t.Fatal(err)
		}
		pc := filepath.Join(dir, "projectContext.md")
		write(t, pc, "# Project Context\n\n"+r.context)
		if err := os.Chtimes(pc, mtime, mtime); err != nil { // "last" in the catalog is this file's mtime; pin it
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(HomeBankDir(), "conventions.md"),
		"# Conventions\n\n- commit messages follow Conventional Commits\n- never hand-edit settings.json\n")

	catalog, err := BuildCatalog(now)
	if err != nil {
		t.Fatal(err)
	}
	checkExample(t, "index.md", catalog)
}

// TestDocsTemplateExamplesBankRegion renders docs/templates/examples/memory/MEMORY.md: a fresh bank
// region (all six typed files present) with one decision and one session-history entry each.
func TestDocsTemplateExamplesBankRegion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "example-repo")

	write(t, filepath.Join(dir, "projectContext.md"), "# Project Context\n\nexample-repo: a sample service used in the docs.\n")
	write(t, filepath.Join(dir, "activeContext.md"), activeContextExample)
	write(t, filepath.Join(dir, "progress.md"), "# Progress\n\n## What Works\n\n- the render pipeline\n")
	write(t, filepath.Join(dir, "conventions.md"), "# Conventions\n\n- keep examples deterministic\n")
	write(t, filepath.Join(dir, "decisionLog.md"), decisionLogExample)
	write(t, filepath.Join(dir, "sessionHistory.md"), sessionHistoryExample)

	checkExample(t, "MEMORY.md", buildBankRegion(dir, now))
}

// TestDocsTemplateExamplesSessionHistory and TestDocsTemplateExamplesDecisionLog and
// TestDocsTemplateExamplesActiveContext check the three typed-file examples in isolation: the shipped
// template (internal/memory/templates/*.md) plus one hand-written fixture entry — `claude-code memory update`'s actual
// append is model-driven, so the fixture entry is kept in this test, not generated through that path.
func TestDocsTemplateExamplesSessionHistory(t *testing.T) {
	t.Parallel()
	checkExample(t, "sessionHistory.md", sessionHistoryExample)
}

func TestDocsTemplateExamplesDecisionLog(t *testing.T) {
	t.Parallel()
	checkExample(t, "decisionLog.md", decisionLogExample)
}

func TestDocsTemplateExamplesActiveContext(t *testing.T) {
	t.Parallel()
	checkExample(t, "activeContext.md", activeContextExample)
}

// decisionLogExample is the shipped template plus one fixture entry (hand-written: `claude-code memory update`'s real
// append is model-driven, not a code path this test can run).
const decisionLogExample = `# Decision Log

> Dated decisions + rationale. APPEND-ONLY, newest first. Supersede, never delete.

## 2026-01-15 — Catalog every embedded template under docs/templates/

Why: a reader auditing ` + "`//go:embed`" + ` directives had no single map from source template to the
file it renders, or a worked example of its output.
`

// sessionHistoryExample is the shipped template plus one fixture entry (same caveat as decisionLogExample).
const sessionHistoryExample = `# Session History

> Newest-first session summaries. APPEND at top, cap 30; overflow → archive/sessionHistory-<yyyy-mm>.md.

## 2026-01-15 — Built the docs/templates/ reference

Indexed every go:embed template in one table and added generated, golden-tested examples under
docs/templates/examples/.
`

// activeContextExample is a filled-in activeContext.md (this file is fully REPLACED each session end,
// so the example shows one plausible filled state rather than template-plus-append).
const activeContextExample = `# Active Context

> Current session state — auto-injected at session start. REPLACE each ` + "`claude-code memory update`" + `.

## Current Focus

Documenting every embedded template under docs/templates/.

## Open Questions

## Blockers

## Next Steps

- Keep the golden examples in sync with their source templates.

## Parked
`
