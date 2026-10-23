package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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

// TestReflectionCommandsNameAnExistingPublicVerb: every commands/reflection/*.md has description +
// argument-hint frontmatter and its body names a `claude-code memory <verb>` that is actually a
// PUBLIC verb in memoryUsage/memorySubcommands — checked against the live verb table, not a
// hardcoded list, so a future rename can't silently drift the launcher out of sync with the CLI.
func TestReflectionCommandsNameAnExistingPublicVerb(t *testing.T) {
	root := reflectionRepoRoot(t)
	dir := filepath.Join(root, "commands", "reflection")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) == 0 {
		t.Fatal("commands/reflection has no launchers")
	}
	verbRe := regexp.MustCompile(`claude-code memory ([a-z-]+)`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if !strings.Contains(body, "description:") {
			t.Errorf("%s: missing description frontmatter", e.Name())
		}
		if !strings.Contains(body, "argument-hint:") {
			t.Errorf("%s: missing argument-hint frontmatter", e.Name())
		}
		matches := verbRe.FindAllStringSubmatch(body, -1)
		if len(matches) == 0 {
			t.Errorf("%s: names no `claude-code memory <verb>`", e.Name())
			continue
		}
		// A launcher's body may legitimately mention OTHER (even private/plumbing) verbs in passing
		// (update.md walks the operator through `memory path`/`memory index` too) — every mentioned
		// verb just needs to be real; only the launcher's OWN verb must be public.
		ownVerb := strings.TrimSuffix(e.Name(), ".md")
		found := false
		for _, m := range matches {
			verb := m[1]
			if _, ok := memorySubcommands[verb]; !ok {
				t.Errorf("%s: names unknown verb %q (not in memorySubcommands)", e.Name(), verb)
				continue
			}
			if verb == ownVerb {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: never names its own verb %q", e.Name(), ownVerb)
		}
		if !strings.Contains(memoryUsage, "memory "+ownVerb) {
			t.Errorf("%s: its own verb %q is not listed as public in memoryUsage", e.Name(), ownVerb)
		}
	}
}

// TestGoReferenceCallsResolveToExistingFiles: every verb Go's memory.Reference(verb) is actually
// called with (grepped from internal/memory source, not hardcoded) must have a matching
// skills/reflection/references/<verb>.md on disk — a missing file is a job that fails loudly at
// runtime, so this catches the drift before that happens.
func TestGoReferenceCallsResolveToExistingFiles(t *testing.T) {
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
		p := filepath.Join(root, "skills", "reflection", "references", verb+".md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("Reference(%q) is called from Go but %s does not exist: %v", verb, p, err)
		}
	}
}
