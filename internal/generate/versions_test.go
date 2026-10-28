package generate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexRestoreFallsBackToLegacyBackupsReadOnly(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	legacy, version := []byte("model = 'plain'\n"), []byte("model = 'versioned'\n")
	files := map[string][]byte{path: []byte("model = 'now'\n"), path + ".backup": legacy, path + ".backup.20250101T000000.000Z": version}
	for p, data := range files {
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := restoreCodexConfig(path, false, &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, version) {
		t.Fatalf("restore did not use newest legacy version: %q", got)
	}
	if !strings.Contains(out.String(), "legacy plaintext backup") {
		t.Fatalf("source not reported: %q", out.String())
	}
	for p, data := range files {
		if p != path {
			if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("legacy file %s was modified or removed: %v", p, err)
			}
		}
	}
}
