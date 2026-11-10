package vscode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLinkExtensionCheckRejectsDeletedSource(t *testing.T) {
	home := extensionHome(t)
	extensions := t.TempDir()
	if _, err := LinkExtension(home, extensions, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "vscode", "claude-code-agents")); err != nil {
		t.Fatal(err)
	}
	_, err := LinkExtension(home, extensions, true)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check of dangling matching link = %v, want missing-source error", err)
	}
}

func TestSyncEscapesTemplatePaths(t *testing.T) {
	hermeticVSCode(t)
	dir := t.TempDir()
	tmpl, dest := filepath.Join(dir, "template.json"), filepath.Join(dir, "settings.json")
	writeFile(t, tmpl, `{"home":"__CLAUDE_HOME__", "bin":"__CLAUDE_BIN__/codex"}`)
	home, bin := `/tmp/home"with\slash`, `/tmp/a"b/bin`
	if _, err := Sync(tmpl, dest, home, bin, false); err != nil {
		t.Fatalf("sync with quoted paths: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("merged JSON: %v", err)
	}
	if got["home"] != home || got["bin"] != bin+"/codex" {
		t.Fatalf("rendered paths = %v, want home %q and bin %q", got, home, bin+"/codex")
	}
}
