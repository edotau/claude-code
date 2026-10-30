package providers

import (
	"encoding/json"
	"slices"

	"github.com/edotau/claude-code/internal/vault"
)

// byokPoolKey is the vault Overlay entry holding the BYOK pool: a JSON array of Databricks wire ids.
const byokPoolKey = "byok-models"

// DatabricksBYOKPool is the seeded BYOK pool of Databricks wire ids, used when the vault overlay is unset or invalid.
var DatabricksBYOKPool = []string{
	"databricks-grok-4-7",
	"databricks-glm-5-3",
	"databricks-gpt-oss-120b",
	"databricks-gpt-6-astra",
	"databricks-gemini-3-8-flash",
	"databricks-gpt-6-1-sol",
}

// BYOKPool is the BYOK model pool: the vault overlay if set and valid, else the DatabricksBYOKPool seed.
func BYOKPool() []string {
	if p, ok := vaultBYOKPool(); ok {
		return p
	}
	return slices.Clone(DatabricksBYOKPool)
}

func vaultBYOKPool() ([]string, bool) {
	var out []string
	var found bool
	err := vault.View(func(t *vault.Tx) error {
		raw, ok := t.Get(vault.Overlay, byokPoolKey)
		if !ok || raw == "" {
			return nil
		}
		found = json.Unmarshal([]byte(raw), &out) == nil && len(out) > 0
		return nil
	})
	if err != nil || !found {
		return nil, false
	}
	return out, true
}
