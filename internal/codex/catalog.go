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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// codexSlug is codex's built-in slug for a menu id, the dotted spelling (gpt-6.1-sol); an unknown id stays as typed.
func codexSlug(id string) string {
	return models.WireOrOriginal("dotted", id)
}

// codexCatalog returns a validated Databricks model catalog path, or lets launch continue without one.
func codexCatalog(ctx context.Context, p *providers.Provider) string {
	if p.Preset != providers.PresetDatabricks {
		return ""
	}
	path, missing, err := codexCatalogPath(ctx, p)
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "codex catalog:", err)
		return ""
	}
	if missing != "" {
		fmt.Fprintln(agentconfig.CatalogWarn, "codex is missing metadata for "+missing+"; update codex")
	}
	return path
}

func codexCatalogPath(ctx context.Context, p *providers.Provider) (string, string, error) {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return "", "", fmt.Errorf("binary lookup failed: %w", err)
	}
	menu, key, prefs, err := catalogPlan(bin, p)
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
	body, missing, err := buildCodexCatalog(p, menu, raw, prefs)
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

// catalogPlan is catalogInputs over the owner's saved preferences, which the rows' defaults follow.
func catalogPlan(bin string, p *providers.Provider) ([]providers.OpencodeModel, string, map[string]string, error) {
	prefs, err := LoadCodexPreferences()
	if err != nil {
		return nil, "", nil, fmt.Errorf("preferences failed: %w", err)
	}
	menu, key, err := catalogInputs(bin, p, prefs)
	return menu, key, prefs, err
}

// catalogRev is bumped when the row builder changes, so cached catalogs from an older builder regenerate.
const catalogRev = "2"

// catalogInputs is the provider's menu and the cache key over codex's binary stamp (no --version spawn), menu and prefs.
func catalogInputs(bin string, p *providers.Provider, prefs map[string]string) ([]providers.OpencodeModel, string, error) {
	stamp, ok := paths.BinKey(bin)
	if !ok {
		return nil, "", fmt.Errorf("binary stamp failed: %s", bin)
	}
	menu, err := providers.OpencodeMenuFor(p)
	if err != nil {
		return nil, "", err
	}
	parts := []string{catalogRev, stamp, p.Name, prefs[codexEffortKey], prefs[extraVerbosity]}
	for _, m := range menu {
		wire := agentconfig.MenuWire(p, m.ID)
		limits, _ := providers.LimitsFor(p, wire) // the listing decides rows and windows, so it is part of the key
		parts = append(parts, wire, m.Label, strconv.FormatBool(m.Responses),
			strconv.FormatBool(codexServes(p, m)), strconv.Itoa(limits.Context))
	}
	rawKey, err := json.Marshal(parts)
	if err != nil {
		return nil, "", fmt.Errorf("cache key failed: %w", err)
	}
	return menu, textutil.SHA256Hex(rawKey), nil
}

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

const catalogTimeout = 30 * time.Second

func runCodexCatalog(ctx context.Context, bin, home string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	spec := agent.EnvSpec{Mode: agent.EnvAgent, Unset: []string{route.OverlayVar}, Set: []string{"CODEX_HOME=" + home}}
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

func buildCodexCatalog(p *providers.Provider, menu []providers.OpencodeModel, raw []byte, prefs map[string]string) ([]byte, string, error) {
	bySlug, byKey, err := indexCodexModels(raw)
	if err != nil {
		return nil, "", err
	}
	var out []map[string]any
	var nativeSlugs []string
	var unbased []providers.OpencodeModel
	labels := make(map[string]int)
	listed := make(map[string]bool)
	add := func(entry map[string]any) {
		applyCodexPreferences(entry, prefs)
		out = append(out, entry)
		listed[entry["slug"].(string)] = true
		labels[entry["display_name"].(string)]++
	}
	for _, m := range menu {
		if !codexServes(p, m) {
			continue
		}
		base := bySlug[codexSlug(m.ID)]
		if base == nil {
			base = byKey[codexModelKey(m.ID)]
		}
		if base == nil {
			unbased = append(unbased, m)
			continue
		}
		add(codexMenuEntry(p, m, base, len(out)))
		nativeSlugs = append(nativeSlugs, base["slug"].(string))
	}
	rows, missing := codexFallbackRows(p, unbased, out, bySlug)
	for _, entry := range rows {
		add(entry)
	}
	if len(out) == 0 {
		return nil, strings.Join(missing, ", "), nil
	}
	for _, entry := range out {
		if labels[entry["display_name"].(string)] > 1 {
			entry["display_name"] = entry["slug"]
		}
	}
	out = appendNativeAliases(out, nativeSlugs, listed)
	body, err := json.Marshal(map[string]any{"models": out})
	return body, strings.Join(missing, ", "), err
}

// fallbackContextWindow is the window a fallback row assumes for a model with no known limit; kept under codex's 272k.
const fallbackContextWindow = 128_000

var gptVersionRE = regexp.MustCompile(`^gpt-(\d+)(?:[.-](\d+))?(?:-|$)`)

// gptVersion is a gpt slug's generation as major*100+minor (gpt-5.4 is 504); ok is false for any other slug.
func gptVersion(slug string) (int, bool) {
	m := gptVersionRE.FindStringSubmatch(codexModelKey(slug))
	if m == nil {
		return 0, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major*100 + minor, true
}

// nearestGPT is the slug of the bundled GPT row closest in generation to want, ties to the lower slug; "" when none.
func nearestGPT(want int, bySlug map[string]map[string]any) string {
	best, bestDiff := "", 0
	for slug := range bySlug {
		got, ok := gptVersion(slug)
		diff := max(want-got, got-want)
		if ok && (best == "" || diff < bestDiff || diff == bestDiff && slug < best) {
			best, bestDiff = slug, diff
		}
	}
	return best
}

// fallbackTemplate is the bundled row of the nearest GPT generation, else the first listed GPT row, else nil.
func fallbackTemplate(id string, listed []map[string]any, bySlug map[string]map[string]any) map[string]any {
	if want, ok := gptVersion(id); ok {
		if best := nearestGPT(want, bySlug); best != "" {
			return bySlug[best]
		}
	}
	for _, row := range listed {
		if models.FamilyKind(row["slug"].(string)) == models.FamilyGPT {
			return row
		}
	}
	return nil
}

// codexFallbackRows builds a row per unbased model from a template; models with no template come back as missing ids.
func codexFallbackRows(p *providers.Provider, unbased []providers.OpencodeModel, listed []map[string]any, bySlug map[string]map[string]any) (rows []map[string]any, missing []string) {
	var assumed []string
	for _, m := range unbased {
		tmpl := fallbackTemplate(m.ID, listed, bySlug)
		if tmpl == nil {
			missing = append(missing, m.ID)
			continue
		}
		entry := codexMenuEntry(p, m, tmpl, len(listed)+len(rows))
		window := fallbackContextWindow
		if l, ok := providers.LimitsFor(p, agentconfig.MenuWire(p, m.ID)); ok && l.Context > 0 {
			window = l.Context
		} else {
			assumed = append(assumed, m.ID)
		}
		entry["context_window"], entry["max_context_window"] = window, window
		rows = append(rows, entry)
	}
	if len(assumed) > 0 {
		fmt.Fprintf(agentconfig.CatalogWarn, "codex catalog: no known context window for %s; assuming %d\n", strings.Join(assumed, ", "), fallbackContextWindow)
	}
	return rows, missing
}

// codexServes: the fresh listing's api_types decide a listed id, else the menu flag or the family rule.
func codexServes(p *providers.Provider, m providers.OpencodeModel) bool {
	wire := agentconfig.MenuWire(p, m.ID)
	if codexExcluded(wire) {
		return false
	}
	if serves, known := providers.ListedResponses(p, wire); known {
		return serves
	}
	return m.Responses || providers.ResponsesServable(p, wire)
}

// codexExcluded: Databricks does not stream Grok over Responses (probed 2026-11-20), though api_types lists it.
func codexExcluded(wire string) bool { return strings.HasPrefix(models.ModelKey(wire), "grok") }

// codexResponsesServable is providers.ResponsesServable less the models Codex cannot run.
func codexResponsesServable(p *providers.Provider, wire string) bool {
	return !codexExcluded(wire) && providers.ResponsesServable(p, wire)
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
	bySlug = make(map[string]map[string]any, len(cat.Models))
	byKey = make(map[string]map[string]any, len(cat.Models))
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

// applyCodexPreferences makes the owner's effort and verbosity the row's defaults where the model supports them.
func applyCodexPreferences(entry map[string]any, prefs map[string]string) {
	if v := prefs[extraVerbosity]; v != "" && entry["support_verbosity"] != false {
		entry["default_verbosity"] = v
	}
	effort := prefs[codexEffortKey]
	levels, _ := entry["supported_reasoning_levels"].([]any)
	for _, level := range levels {
		if row, _ := level.(map[string]any); effort != "" && row["effort"] == effort {
			entry["default_reasoning_level"] = effort
			return
		}
	}
}

func codexModelKey(slug string) string {
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

// codexResponsesModel is the default a model Responses cannot serve falls back to: the menu's first Responses model.
func codexResponsesModel(p *providers.Provider, model string) string {
	if p.Preset != providers.PresetDatabricks || codexResponsesServable(p, model) {
		return model
	}
	return cmp.Or(providers.FirstResponsesModel(p), model)
}

// codexRefuse is the usage error for a typed model codex cannot run over Responses, or nil when it can.
func codexRefuse(p *providers.Provider, model string) error {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	if codexResponsesServable(p, model) {
		return nil
	}
	hint := cmp.Or(providers.FirstResponsesModel(p), "a Responses model")
	return &agent.UsageError{Msg: fmt.Sprintf("codex: %s is not served over Responses on profile %s; try %s", model, p.Name, hint)}
}
