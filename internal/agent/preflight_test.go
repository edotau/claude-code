package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestCheckModel1MAliasPassesLikeBareAlias: codex on a Databricks profile treats <alias>[1m] exactly as the bare alias.
func TestCheckModel1MAliasPassesLikeBareAlias(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Host: "https://dev.example.com"}
	now := time.Now()
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		bareWire, bareFailed, bareNote := CheckModel("codex", p, alias, now)
		wire, failed, note := CheckModel("codex", p, alias+"[1m]", now)
		if failed != nil || bareFailed != nil || note != bareNote || wire != alias+"[1m]" || bareWire != alias {
			t.Errorf("%s[1m]: wire %q failed %v note %q; bare %s: wire %q failed %v note %q",
				alias, wire, failed, note, alias, bareWire, bareFailed, bareNote)
		}
	}
}
