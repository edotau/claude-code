package vendors

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

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

// digitHyphen matches the hyphen in "6-1"; codex's slugs spell it gpt-6.1-sol.
var digitHyphen = regexp.MustCompile(`(\d)-(\d)`)

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
	for _, m := range providers.DatabricksOpencodeMenu {
		base, ok := bySlug[digitHyphen.ReplaceAllString(m.ID, "$1.$2")]
		if !ok { // codex already reads this catalog: its dump lists our wire ids, not its built-ins
			base, ok = bySlug[p.ModelPrefix+m.ID]
		}
		if !m.Responses || !ok {
			continue
		}
		e := make(map[string]any, len(base))
		for k, v := range base {
			e[k] = v
		}
		e["slug"], e["display_name"] = p.ModelPrefix+m.ID, m.Label+" · "+p.Name
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
