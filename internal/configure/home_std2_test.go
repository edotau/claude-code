package configure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStd2WebSearchCodexHomeNoPlaintextBackup(t *testing.T) {
	cfgDir, home, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	custom := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(custom, []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reg func(string) error
	for _, c := range webSearchClients() {
		if c.name == "codex" {
			reg = c.register
		}
	}
	if reg == nil {
		t.Fatal("no codex client")
	}
	if err := reg("/bin/claude-code"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(custom)
	if !strings.Contains(string(got), "[mcp_servers.web_search]") {
		t.Errorf("$CODEX_HOME/config.toml not edited: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); err == nil {
		t.Error("~/.codex/config.toml was written despite CODEX_HOME")
	}
	var leftovers []string
	filepath.WalkDir(cfgDir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && strings.Contains(filepath.Base(p), "config.toml") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	entries, _ := os.ReadDir(codexHome)
	for _, e := range entries {
		if e.Name() != "config.toml" {
			leftovers = append(leftovers, e.Name())
		}
	}
	if len(leftovers) > 0 {
		t.Errorf("plaintext backup left behind: %v", leftovers)
	}
}
