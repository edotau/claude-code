package memorycmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSkillVerbsArePublicCLIVerbs checks the skill's verb table against memoryPage and memorySubcommands.
func TestSkillVerbsArePublicCLIVerbs(t *testing.T) {
	table := reflectionVerbTable(t)
	verbRe := regexp.MustCompile("^`([a-z][a-z-]*)")
	seen := map[string]bool{}
	for line := range strings.SplitSeq(table, "\n") {
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
		if _, ok := memorySubcommands(nil)[verb]; !ok {
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

func reflectionVerbTable(t *testing.T) string {
	t.Helper()
	root := reflectionRepoRoot(t)
	path := filepath.Join(root, "skills", "reflection", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(data)
	tableStart := strings.Index(body, "| Verb |")
	if tableStart < 0 {
		t.Fatal("SKILL.md has no verb table (expected a \"| Verb |\" header row)")
	}
	tableEnd := strings.Index(body[tableStart:], "\n\n")
	if tableEnd < 0 {
		tableEnd = len(body) - tableStart
	}
	return body[tableStart : tableStart+tableEnd]
}

// reflectionRepoRoot resolves this repo's toplevel from the memorycmd package's working directory.
func reflectionRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..", "..")
}
