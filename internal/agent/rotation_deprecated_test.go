package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A model on the picker's drop list leaves the rotation candidates; undrop brings it back.
func TestWithoutDeprecatedSkipsDroppedModels(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dev", Provider: "databricks", Preset: providers.PresetDatabricks,
		Host: "https://dev.example.com", Auth: providers.Auth{Type: providers.AuthOAuth}}
	cands := []catalog.Candidate{{Model: "databricks-gpt-5-mini"}, {Model: "databricks-claude-sonnet-4-5"}}
	if _, err := providers.RecordDeprecated(p, "databricks-gpt-5-mini", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := withoutDeprecated(p, append([]catalog.Candidate(nil), cands...)); len(got) != 1 || got[0].Model != cands[1].Model {
		t.Fatalf("pool = %v, want only %s", got, cands[1].Model)
	}
	if _, err := providers.EditPickerDrops(p, []string{"databricks-gpt-5-mini"}, true, true); err != nil {
		t.Fatal(err)
	}
	if got := withoutDeprecated(p, append([]catalog.Candidate(nil), cands...)); len(got) != 2 {
		t.Fatalf("pool after undrop = %v, want both", got)
	}
}
