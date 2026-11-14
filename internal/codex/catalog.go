package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/textutil"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// codexSlug is codex's own built-in slug for a menu id: the dotted spelling (gpt-6.1-sol); an unknown id stays as typed.
func codexSlug(id string) string {
	return models.WireOrOriginal("dotted", id)
}

// codexCatalogArgs returns a validated Databricks model catalog override, or lets launch continue without one.
func codexCatalogArgs(ctx context.Context, p *providers.Provider) []string {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	path, missing, err := codexCatalogPath(ctx, p)
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "codex catalog:", err)
		return nil
	}
	if missing != "" {
		fmt.Fprintln(agentconfig.CatalogWarn, "codex is missing metadata for "+missing+"; update codex")
	}
	if path == "" {
		return nil
	}
	return []string{"-c", "model_catalog_json=" + agentconfig.TOMLQuote(path)}
}

func codexCatalogPath(ctx context.Context, p *providers.Provider) (string, string, error) {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return "", "", fmt.Errorf("binary lookup failed: %w", err)
	}
	menu, key, err := catalogInputs(bin, p)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(paths.StateDir(), "codex")
	path := filepath.Join(dir, "catalog-"+p.Name+".json")
	keyPath := filepath.Join(dir, "catalog-"+p.Name+".key")
	if catalogCached(path, keyPath, key) {
		return path, "", nil
	}
	home, err := catalogTempDir()
	if err != nil {
		return "", "", fmt.Errorf("temporary home failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	raw, err := runCodexCatalog(ctx, bin, home, "debug", "models", "--bundled")
	if err != nil {
		return "", "", fmt.Errorf("bundled dump failed: %w", err)
	}
	body, missing, err := buildCodexCatalog(p, menu, raw)
	if err != nil {
		return "", "", fmt.Errorf("bundled metadata failed: %w", err)
	}
	if body == nil {
		return "", missing, nil
	}
	if err := validateCodexCatalog(ctx, bin, home, body); err != nil {
		return "", "", err
	}
	if err := storeCodexCatalog(dir, path, keyPath, body, key); err != nil {
		return "", "", err
	}
	return path, missing, nil
}

// catalogTempDir makes the scratch CODEX_HOME only a catalog cache miss needs; a var so tests catch a stray one.
var catalogTempDir = func() (string, error) { return os.MkdirTemp("", "codex-catalog-") }

// catalogInputs is the provider's menu and the cache key over codex's binary stamp (no --version spawn) and that menu.
func catalogInputs(bin string, p *providers.Provider) ([]providers.OpencodeModel, string, error) {
	stamp, ok := paths.BinKey(bin)
	if !ok {
		return nil, "", fmt.Errorf("binary stamp failed: %s", bin)
	}
	menu, err := providers.OpencodeMenuFor(p)
	if err != nil {
		return nil, "", err
	}
	parts := []string{stamp, p.Name}
	for _, m := range menu {
		parts = append(parts, agentconfig.MenuWire(p, m.ID), m.Label, strconv.FormatBool(m.Responses))
	}
	rawKey, err := json.Marshal(parts)
	if err != nil {
		return nil, "", fmt.Errorf("cache key failed: %w", err)
	}
	return menu, textutil.SHA256Hex(rawKey), nil
}

// catalogCached reports a stored catalog file whose key file matches key.
func catalogCached(path, keyPath, key string) bool {
	cached, err := os.ReadFile(keyPath)
	if err != nil || string(cached) != key {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// validateCodexCatalog has codex load body as a model catalog from a scratch file in home.
func validateCodexCatalog(ctx context.Context, bin, home string, body []byte) error {
	candidate := filepath.Join(home, "catalog.json")
	if err := os.WriteFile(candidate, body, 0o600); err != nil {
		return fmt.Errorf("candidate write failed: %w", err)
	}
	if _, err := runCodexCatalog(ctx, bin, home, "-c", "model_catalog_json="+agentconfig.TOMLQuote(candidate), "debug", "models"); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	return nil
}

// storeCodexCatalog writes the catalog and its key under dir as one locked pair.
func storeCodexCatalog(dir, path, keyPath string, body []byte, key string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("directory creation failed: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("directory permissions failed: %w", err)
	}
	return paths.WithFileLock(path, func() error { // the catalog and its key must change as one pair
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cache key invalidation failed: %w", err)
		}
		if err := paths.AtomicWrite(path, body, 0o600); err != nil {
			return fmt.Errorf("catalog write failed: %w", err)
		}
		if err := paths.AtomicWrite(keyPath, []byte(key), 0o600); err != nil {
			return fmt.Errorf("cache key write failed: %w", err)
		}
		return nil
	})
}

// catalogTimeout bounds one `codex debug models` run.
const catalogTimeout = 30 * time.Second

func runCodexCatalog(ctx context.Context, bin, home string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	spec := agent.EnvSpec{Mode: agent.EnvAgent, Unset: []string{agentconfig.OverlayVar}, Set: []string{"CODEX_HOME=" + home}}
	cmd.Env = spec.Environ(agent.ProbeContext(ctx, "")) // no login or route credential; unset rides ctx
	agentconfig.RecordOverlayDebug("codex-catalog", cmd.Env)
	cmd.Dir = home
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

var (
	_ = schemas.Register(schemas.Format{
		Name: "codex-catalog", Owner: "internal/codex", Kind: schemas.Derived,
		Glob:    func() string { return filepath.Join(paths.StateDir(), "codex", "catalog-*.json") },
		Version: 1, Keep: "rebuilt from the provider catalog; codex reads it, so no schema field",
	})
)

func buildCodexCatalog(p *providers.Provider, menu []providers.OpencodeModel, raw []byte) ([]byte, string, error) {
	bySlug, byKey, err := indexCodexModels(raw)
	if err != nil {
		return nil, "", err
	}
	var out []map[string]any
	var nativeSlugs []string
	var missing string
	labels := make(map[string]int)
	listed := make(map[string]bool)
	for _, m := range menu {
		if !m.Responses {
			continue
		}
		base := bySlug[codexSlug(m.ID)]
		if base == nil {
			base = byKey[codexModelKey(m.ID)]
		}
		if base == nil {
			if missing == "" {
				missing = m.ID
			}
			continue
		}
		entry := codexMenuEntry(p, m, base, len(out))
		out = append(out, entry)
		nativeSlugs = append(nativeSlugs, base["slug"].(string))
		listed[entry["slug"].(string)] = true
		labels[entry["display_name"].(string)]++
	}
	if len(out) == 0 {
		return nil, missing, nil
	}
	for _, entry := range out {
		if labels[entry["display_name"].(string)] > 1 {
			entry["display_name"] = entry["slug"]
		}
	}
	out = appendNativeAliases(out, nativeSlugs, listed)
	body, err := json.Marshal(map[string]any{"models": out})
	return body, missing, err
}

// appendNativeAliases adds a hidden row under each base model's own slug so codex still resolves it.
func appendNativeAliases(out []map[string]any, nativeSlugs []string, listed map[string]bool) []map[string]any {
	for i, native := range nativeSlugs {
		if listed[native] {
			continue
		}
		alias := cloneCodexMetadata(out[i]).(map[string]any)
		alias["slug"], alias["visibility"] = native, "hide"
		out = append(out, alias)
		listed[native] = true
	}
	return out
}

// indexCodexModels parses codex's bundled catalog into lookups by exact slug and by normalized key.
func indexCodexModels(raw []byte) (bySlug, byKey map[string]map[string]any, err error) {
	var cat struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, nil, err
	}
	if len(cat.Models) == 0 {
		return nil, nil, errors.New("no models")
	}
	bySlug, byKey = make(map[string]map[string]any), make(map[string]map[string]any)
	for _, m := range cat.Models {
		slug, ok := m["slug"].(string)
		if !ok || slug == "" {
			return nil, nil, errors.New("model has no string slug")
		}
		bySlug[slug], byKey[codexModelKey(slug)] = m, m
	}
	return bySlug, byKey, nil
}

// codexMenuEntry clones base's metadata as a listed menu row at the given priority.
func codexMenuEntry(p *providers.Provider, m providers.OpencodeModel, base map[string]any, priority int) map[string]any {
	entry := cloneCodexMetadata(base).(map[string]any)
	entry["slug"], entry["display_name"] = agentconfig.MenuWire(p, m.ID), m.Label+" · "+p.Name
	entry["visibility"], entry["supported_in_api"], entry["priority"] = "list", true, priority
	entry["service_tiers"], entry["additional_speed_tiers"] = []any{}, []any{}
	entry["upgrade"], entry["availability_nux"] = nil, nil
	return entry
}

func codexModelKey(slug string) string {
	slug = strings.TrimPrefix(slug, "system.ai.")
	slug = strings.TrimPrefix(slug, "databricks-")
	return strings.ReplaceAll(slug, ".", "-")
}

func cloneCodexMetadata(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = cloneCodexMetadata(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneCodexMetadata(item)
		}
		return out
	default:
		return value
	}
}

// codexResponsesModel swaps a model Responses cannot serve (opus alias, gpt-oss) for the menu's first Responses model.
func codexResponsesModel(p *providers.Provider, model string) string {
	if p.Preset != providers.PresetDatabricks || providers.ResponsesServable(p, model) {
		return model
	}
	return cmp.Or(providers.FirstResponsesModel(p), model)
}
