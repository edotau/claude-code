package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildIndexNests: a flat path list costs one token per path segment repeated on every line. A
// nested tree collapses shared prefixes, which is what lets a whole repo fit where inlining files
// cannot — the gemini's "max-files-exceeded" failure was exactly this budget running out.
func TestBuildIndexNests(t *testing.T) {
	idx := BuildIndex(Context{
		Included: []IncludedFile{{Path: "internal/router/leg.go", Bytes: 100}},
		Skipped:  []SkippedFile{{Path: "internal/cli/run.go", Reason: "max-files-exceeded"}},
	})
	var tree map[string]any
	if err := json.Unmarshal([]byte(idx), &tree); err != nil {
		t.Fatalf("index is not valid JSON: %v\n%s", err, idx)
	}
	internal, ok := tree["internal"].(map[string]any)
	if !ok {
		t.Fatalf("index is not nested by directory: %s", idx)
	}
	if _, ok := internal["router"].(map[string]any); !ok {
		t.Errorf("second level missing: %s", idx)
	}
}

// TestIndexCoversSkippedFiles: a file that was too big to inline must STILL appear in the index.
// Omitting it is what made the gemini answer "contents are not available" — it could not even see
// that the file existed, so it could not ask for it by name.
func TestIndexCoversSkippedFiles(t *testing.T) {
	idx := BuildIndex(Context{Skipped: []SkippedFile{{Path: "internal/router/spillpool.go", Reason: "max-files-exceeded"}}})
	if !strings.Contains(idx, "spillpool.go") {
		t.Errorf("skipped file absent from the index — it becomes invisible, not merely unread:\n%s", idx)
	}
}

// TestIndexCarriesMetadata: the index is a routing aid, so each leaf needs enough to decide whether
// the file is worth pulling — size, and whether its contents were actually included.
func TestIndexCarriesMetadata(t *testing.T) {
	idx := BuildIndex(Context{Included: []IncludedFile{{Path: "a/b.go", Bytes: 4096}}})
	for _, want := range []string{"4096", "b.go"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index leaf missing %q:\n%s", want, idx)
		}
	}
}
