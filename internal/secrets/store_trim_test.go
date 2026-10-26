package secrets

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Paste debris at either end is trimmed on every write path; a newline inside the value is still refused.
func TestStoreTrimsPasteDebris(t *testing.T) {
	vaulttest.Use(t)
	for _, raw := range []string{"tok-123\n", "tok-123\r\n", "  tok-123 \t\n\n", "\ntok-123"} {
		if err := Store("PROVIDER_X_REFRESH_TOKEN", raw); err != nil {
			t.Fatalf("Store(%q) = %v, want the debris trimmed", raw, err)
		}
		got, _, err := Lookup("PROVIDER_X_REFRESH_TOKEN")
		if err != nil || got != "tok-123" {
			t.Fatalf("Store(%q) then Lookup = %q, %v; want %q", raw, got, err, "tok-123")
		}
	}
	if err := Store("PROVIDER_X_REFRESH_TOKEN", "tok\n123"); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("Store with an inner newline = %v, want the one-line refusal", err)
	}
}
