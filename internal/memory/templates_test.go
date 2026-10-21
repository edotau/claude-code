package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// oldTemplates is store.go's pre-embed literal content (d3f9260), pinned so the embed cutover cannot drift bytes.
var oldTemplates = map[string]string{
	"projectContext.md": "# Project Context\n\n" +
		"> Mostly-static orientation. Points at canonical docs — never duplicates them.\n\n" +
		"(Describe the project in a line or two; link CLAUDE.md/README.)\n",
	"activeContext.md": "# Active Context\n\n" +
		"> Current session state — auto-injected at session start. REPLACE each `/memory:end`.\n\n" +
		"## Current Focus\n\n## Open Questions\n\n## Blockers\n\n## Next Steps\n\n## Parked\n",
	"progress.md": "# Progress\n\n" +
		"> Task tracking — auto-injected at session start. REPLACE each `/memory:end`.\n\n" +
		"## What Works\n\n## In Progress\n\n## What's Next\n\n## Known Issues\n",
	"decisionLog.md": "# Decision Log\n\n" +
		"> Dated decisions + rationale. APPEND-ONLY, newest first. Supersede, never delete.\n",
	"conventions.md": "# Conventions\n\n" +
		"> Learned patterns/preferences. APPEND one imperative bullet each; prune what no longer holds.\n",
	"sessionHistory.md": "# Session History\n\n" +
		"> Newest-first session summaries. APPEND at top, cap 30; overflow → archive/sessionHistory-<yyyy-mm>.md.\n",
}

// TestTemplatesMapByteIdentical pins the package's templates() map against the pre-embed literals.
func TestTemplatesMapByteIdentical(t *testing.T) {
	m := templates()
	if len(m) != len(oldTemplates) {
		t.Fatalf("templates() has %d entries, want %d", len(m), len(oldTemplates))
	}
	for name, want := range oldTemplates {
		got, ok := m[name]
		if !ok {
			t.Errorf("templates missing %q", name)
			continue
		}
		if got != want {
			t.Errorf("templates[%q] changed:\ngot  %q\nwant %q", name, got, want)
		}
	}
}

// TestEmbeddedTemplateFilesByteIdentical reads internal/memory/templates/*.md off disk directly, so the
// assertion holds regardless of how the embed.FS is wired into the templates map.
func TestEmbeddedTemplateFilesByteIdentical(t *testing.T) {
	for name, want := range oldTemplates {
		p := filepath.Join("templates", name)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(data) != want {
			t.Errorf("templates/%s changed:\ngot  %q\nwant %q", name, string(data), want)
		}
	}
}
