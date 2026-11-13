package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCore(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPortableCoreExcludesScopedIncludesStandardOnce(t *testing.T) {
	root := writeCore(t, map[string]string{
		"CLAUDE.md":                  "# root\n",
		"rules/standards/b.md":       "# b\n",
		"rules/standards/a.md":       "# a\n",
		"rules/standards/scoped.md":  "---\npaths:\n  - \"**/*.go\"\n---\n# SENTINEL\n",
		"rules/software/go/other.md": "# other\n",
	})
	got, err := PortableCore(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CLAUDE.md", "rules/standards/a.md", "rules/standards/b.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != filepath.Join(root, w) {
			t.Errorf("got[%d]=%s, want %s", i, got[i], w)
		}
	}
	out, _, err := renderDocs(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SENTINEL") || strings.Count(out, "## a") != 1 {
		t.Errorf("render wrong:\n%s", out)
	}
}

func TestPortableCoreMissingFileNamesIt(t *testing.T) {
	root := writeCore(t, map[string]string{"rules/standards/a.md": "# a\n"})
	if _, err := PortableCore(root); err == nil || !strings.Contains(err.Error(), "CLAUDE.md") {
		t.Fatalf("want error naming CLAUDE.md, got %v", err)
	}
	root = writeCore(t, map[string]string{"CLAUDE.md": "x", "rules/standards/a.md": "# a\n"})
	if err := os.Chmod(filepath.Join(root, "rules/standards/a.md"), 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	if _, err := PortableCore(root); err == nil || !strings.Contains(err.Error(), "a.md") {
		t.Fatalf("want error naming a.md, got %v", err)
	}
}

func TestExternalDropsMatchRealFiles(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, d := range externalDrops {
		p := filepath.Join(root, d.file)
		if d.file != "CLAUDE.md" {
			p = filepath.Join(root, "rules", "standards", d.file)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		want := d.heading + "\n"
		if d.linePrefix != "" {
			want = d.linePrefix
		}
		if !strings.Contains("\n"+string(raw), "\n"+want) {
			t.Errorf("%s: %q not found; update externalDrops", d.file, strings.TrimSpace(want))
		}
	}
}
