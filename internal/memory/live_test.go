package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// LiveTranscript prefers projects/<slug(cwd)>/<id>.jsonl, falls back to any project, and is "" otherwise.
func TestLiveTranscript(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	cwd := t.TempDir()
	write := func(slug, id string) string {
		p := filepath.Join(cfg, "projects", slug, id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exact := write(ProjectSlug(cwd), "s1")
	write("-other-project", "s1")
	other := write("-other-project", "s2")
	if got := LiveTranscript(cwd, "s1"); got != exact {
		t.Errorf("exact: got %q, want %q", got, exact)
	}
	if got := LiveTranscript(cwd, "s1.jsonl"); got != exact {
		t.Errorf("exact with .jsonl: got %q, want %q", got, exact)
	}
	if got := LiveTranscript(cwd, "s2"); got != other {
		t.Errorf("glob fallback: got %q, want %q", got, other)
	}
	for _, id := range []string{"", "missing"} {
		if got := LiveTranscript(cwd, id); got != "" {
			t.Errorf("id %q: got %q, want \"\"", id, got)
		}
	}
}
