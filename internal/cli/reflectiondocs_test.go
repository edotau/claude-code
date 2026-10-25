package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// reflectionRepoRoot resolves this repo's toplevel from internal/cli's own working directory.
func reflectionRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

// TestGoReferenceCallsResolveToEmbeddedPrompts: every verb Go's memory.Reference(verb) is actually
// called with (grepped from internal/memory source, not hardcoded) resolves to a non-empty
// embedded prompt — commands/reflection and skills/reflection/references are gone, so this is the
// drift check that used to compare against on-disk files.
func TestGoReferenceCallsResolveToEmbeddedPrompts(t *testing.T) {
	root := reflectionRepoRoot(t)
	memDir := filepath.Join(root, "internal", "memory")
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatalf("read %s: %v", memDir, err)
	}
	callRe := regexp.MustCompile(`Reference\("([a-z-]+)"\)`)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range callRe.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("no memory.Reference(\"<verb>\") calls found in internal/memory — regex drifted or the API moved")
	}
	for verb := range seen {
		got, err := memory.Reference(verb)
		if err != nil {
			t.Errorf("Reference(%q) is called from Go but errors: %v", verb, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Reference(%q) is called from Go but resolves to an empty prompt", verb)
		}
	}
}

// TestSkillVerbsArePublicCLIVerbs checks the skill's verb table against memoryPage and memorySubcommands.
func TestSkillVerbsArePublicCLIVerbs(t *testing.T) {
	root := reflectionRepoRoot(t)
	path := filepath.Join(root, "skills", "reflection", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	verbRe := regexp.MustCompile("^`([a-z][a-z-]*)")
	tableStart := strings.Index(body, "| Verb |")
	if tableStart < 0 {
		t.Fatal("SKILL.md has no verb table (expected a \"| Verb |\" header row)")
	}
	tableEnd := strings.Index(body[tableStart:], "\n\n")
	if tableEnd < 0 {
		tableEnd = len(body) - tableStart
	}
	table := body[tableStart : tableStart+tableEnd]
	seen := map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 2 {
			continue
		}
		first := strings.TrimSpace(cells[1]) // cells[0] is empty (row starts with "|")
		m := verbRe.FindStringSubmatch(first)
		if m == nil {
			continue
		}
		verb := m[1]
		if seen[verb] {
			continue
		}
		seen[verb] = true
		if _, ok := memorySubcommands[verb]; !ok {
			t.Errorf("SKILL.md names unknown verb %q (not in memorySubcommands)", verb)
			continue
		}
		if !helpPageHasCommand(memoryPage, verb) {
			t.Errorf("SKILL.md names verb %q that is not listed as public in memoryPage", verb)
		}
	}
	if len(seen) == 0 {
		t.Fatal("verb table names no verbs")
	}
}
