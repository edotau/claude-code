package opencode

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOpencodeRulesOnUsesPortableCoreWithoutClaudeMD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	std := filepath.Join(dir, "rules", "standards")
	if err := os.MkdirAll(std, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(dir, "CLAUDE.md"): "# c\n",
		filepath.Join(std, "b.md"):      "# b\n",
		filepath.Join(std, "a.md"):      "# a\n",
		filepath.Join(std, "scoped.md"): "---\npaths: [\"*.go\"]\n---\n# s\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{}
	opencodeRulesOn(cfg)
	want := []string{filepath.Join(std, "a.md"), filepath.Join(std, "b.md")}
	if got := cfg["instructions"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("instructions = %v, want %v", got, want)
	}
}
