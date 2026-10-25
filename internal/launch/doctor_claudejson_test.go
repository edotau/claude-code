package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
	}
}
