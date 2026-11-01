package bank

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGoReferenceCallsResolveToEmbeddedPrompts: every verb Go's Reference(verb) is actually
// called with (grepped from internal/memory source, not hardcoded) resolves to a non-empty
// embedded prompt — commands/reflection and skills/reflection/references are gone, so this is the
// drift check that used to compare against on-disk files.
func TestGoReferenceCallsResolveToEmbeddedPrompts(t *testing.T) {
	memDir := ".."
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
		t.Fatal("no Reference(\"<verb>\") calls found in internal/memory — regex drifted or the API moved")
	}
	for verb := range seen {
		got, err := Reference(verb)
		if err != nil {
			t.Errorf("Reference(%q) is called from Go but errors: %v", verb, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Reference(%q) is called from Go but resolves to an empty prompt", verb)
		}
	}
}
