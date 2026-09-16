package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallMergesIntoConfigDir(t *testing.T) {
	hermetic(t, "")
	self := filepath.Join(t.TempDir(), "claude-code")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	selfBinary = func() (string, error) { return self, nil }
	file := SettingsFile()
	orig := `{"theme": "dark", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "orca stop"}]}]}}`
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Install(&out, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Fatal("--dry-run wrote settings.json")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(file), "bin", "claude")); err == nil {
		t.Fatal("--dry-run created shims")
	}
	if err := Install(&out, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	for _, want := range []string{`"theme": "dark"`, `"orca stop"`, "hook stop-format", "hook safety"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("merged settings missing %s:\n%s", want, b)
		}
	}
	if link, _ := os.Readlink(filepath.Join(filepath.Dir(file), "bin", "codex")); link != "claude-code" {
		t.Errorf("codex shim -> %q", link)
	}
	baks, _ := filepath.Glob(file + ".bak-*")
	if len(baks) != 1 {
		t.Errorf("backups %v", baks)
	}
	if has, err := settingsHasHooks(file); !has || err != nil {
		t.Errorf("doctor hook check: %v %v", has, err)
	}
}
