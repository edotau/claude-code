package crossgen

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// A skip-listed source still exists and is deliberately not projected; an orphan's source is gone. Reporting
// both as "orphaned: no source" reads as data loss for what is a routine skip.
func TestPruneDistinguishesSkipListedFromOrphaned(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"approval-gate", "ghost"} {
		writeFile(t, filepath.Join(root, name, "SKILL.md"), agentSkillBannerPrefix+"\n\n# "+name+"\n")
	}
	skills := []Source{{dirName: "approval-gate", frontmatter: map[string]any{"name": "approval-gate"}}}

	var out bytes.Buffer
	pruneOrphans(&out, nil, skills, projectedNames(nil, skills), root, true)

	got := out.String()
	if !strings.Contains(got, "not projected to gemini: 'approval-gate' is Claude-Code-only") {
		t.Errorf("a skip-listed skill must be reported as not-projected:\n%s", got)
	}
	if strings.Contains(got, "orphaned: no source 'approval-gate'") {
		t.Errorf("skip-listed skill still reported as orphaned:\n%s", got)
	}
	if !strings.Contains(got, "orphaned: no source 'ghost'") {
		t.Errorf("a source-less dir must still be called orphaned:\n%s", got)
	}
}
