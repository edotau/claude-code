package providers

import (
	"encoding/json"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

func readOverlay(t *testing.T, key string) string {
	t.Helper()
	var s string
	if err := vault.View(func(tx *vault.Tx) error { s, _ = tx.Get(vault.Overlay, key); return nil }); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUpdateOverlayDocDryRunWritesNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderModel("personal", "haiku", "seed-x"); err != nil {
		t.Fatal(err)
	}
	before, prevBefore := readOverlay(t, "providers"), readOverlay(t, "providers.prev")
	mutate := func(top map[string]json.RawMessage, _ *vault.Tx) error {
		top["marker"] = json.RawMessage(`"changed"`)
		return nil
	}
	oldDoc, newDoc, err := UpdateOverlayDoc(false, mutate)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldDoc) != before || string(newDoc) == before || !json.Valid(newDoc) {
		t.Fatalf("dry run docs wrong: old=%q new=%q", oldDoc, newDoc)
	}
	if got := readOverlay(t, "providers"); got != before {
		t.Errorf("dry run wrote providers: %q", got)
	}
	if got := readOverlay(t, "providers.prev"); got != prevBefore {
		t.Errorf("dry run wrote providers.prev: %q", got)
	}

	if _, _, err := UpdateOverlayDoc(true, mutate); err != nil {
		t.Fatal(err)
	}
	if got := readOverlay(t, "providers.prev"); got != before {
		t.Errorf("apply: providers.prev = %q, want old doc", got)
	}
	if got := readOverlay(t, "providers"); got != string(newDoc) {
		t.Errorf("apply: providers = %q, want %q", got, newDoc)
	}
}
