package vendors

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// codexDebugModels dumps codex's own catalog (`codex debug models`); tests replace it.
var codexDebugModels = func() ([]byte, error) {
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "debug", "models").Output()
}

// menuWire spells a bare menu id the way p serves it; an id outside the catalog still takes the row's prefix once.
func menuWire(p *providers.Provider, id string) string {
	if w := p.WireModel(id); w != id {
		return w
	}
	rule := p.SpellingRule()
	if _, _, known := models.Resolve(id); known || rule == "" ||
		strings.HasPrefix(id, models.DatabricksPrefix) || strings.HasPrefix(id, "system.ai.") {
		return id
	}
	return models.Wire(rule, id)
}

// codexSlug is codex's own built-in slug for a menu id: the dotted spelling (gpt-6.1-sol); an unknown id stays as typed.
func codexSlug(id string) string {
	if canonical, _, ok := models.Resolve(id); ok {
		return models.Wire("dotted", canonical)
	}
	return id
}

// codexCatalogArgs writes a codex model catalog of a Databricks row's GPT models (cloned from codex's own entries,
// renamed to wire ids, no service tiers or upgrade prompts) and returns its -c flag; nil when it does not apply.
func codexCatalogArgs(p *providers.Provider) []string {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	raw, err := codexDebugModels()
	if err != nil {
		return nil
	}
	var cat struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(raw, &cat) != nil {
		return nil
	}
	bySlug := map[string]map[string]any{}
	for _, m := range cat.Models {
		if s, ok := m["slug"].(string); ok {
			bySlug[s] = m
		}
	}
	var out []map[string]any
	for _, m := range providers.OpencodeMenu() {
		wire := menuWire(p, m.ID)
		base, ok := bySlug[codexSlug(m.ID)]
		if !ok { // codex already reads this catalog: its dump lists our wire ids, not its built-ins
			base, ok = bySlug[wire]
		}
		if !m.Responses || !ok {
			continue
		}
		e := make(map[string]any, len(base))
		for k, v := range base {
			e[k] = v
		}
		e["slug"], e["display_name"] = wire, m.Label+" · "+p.Name
		e["service_tiers"], e["additional_speed_tiers"], e["upgrade"], e["availability_nux"] = []any{}, []any{}, nil, nil
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"models": out})
	if err != nil {
		return nil
	}
	path := filepath.Join(paths.StateDir(), "codex", "catalog-"+p.Name+".json")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || os.WriteFile(path, body, 0o600) != nil {
		return nil
	}
	return []string{"-c", "model_catalog_json=" + tomlStr(path)}
}
