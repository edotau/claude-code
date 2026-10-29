package plans

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ClaimFree leaves a live claim alone and ClearClaimOwned clears only its owner's claim.
func TestClaimFreeAndClearOwned(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := filepath.Join(t.TempDir(), "2026-10-29-x.md")
	if err := os.WriteFile(p, []byte("- [ ] 1.2 task owns: a.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClaimFree(p, "1.2", "working: Agent u1 · since 10:00"); err != nil {
		t.Fatal(err)
	}
	if err := ClaimFree(p, "1.2", "working: Agent u2 · since 10:01"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "u1") || strings.Contains(string(b), "u2") {
		t.Fatalf("second ClaimFree replaced the first claim:\n%s", b)
	}
	if err := ClearClaimOwned(p, "1.2", " u2 "); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "u1") {
		t.Fatalf("a non-owner cleared the claim:\n%s", b)
	}
	if err := ClearClaimOwned(p, "1.2", " u1 "); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "working:") {
		t.Fatalf("owner did not clear:\n%s", b)
	}
}
