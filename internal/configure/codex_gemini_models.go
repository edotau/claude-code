// codex_gemini_models.go — the codex and gemini agent pages' own Model row: each vendor's own CLI
// catalog (never the shared github-copilot provider table), Effort first, same picker shape as copilot's.
package configure

import (
	"cmp"
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// vendorCatalogTimeout bounds a vendor CLI's own model-listing call.
const vendorCatalogTimeout = 30 * time.Second

// --- codex: `codex debug models` -------------------------------------------------------------

// codexModelInfo is one listed model from `codex debug models`.
type codexModelInfo struct {
	Slug, DisplayName, DefaultEffort string
	ContextWindow                    int
	Efforts                          []string
}

type codexDebugModelsOutput struct {
	Models []struct {
		Slug                     string `json:"slug"`
		DisplayName              string `json:"display_name"`
		Visibility               string `json:"visibility"`
		ContextWindow            int    `json:"context_window"`
		DefaultReasoningLevel    string `json:"default_reasoning_level"`
		SupportedReasoningLevels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	} `json:"models"`
}

// codexListModelsFn runs `codex debug models`, keeping only visibility=="list" entries; a seam for tests.
var codexListModelsFn = func(ctx context.Context) ([]codexModelInfo, error) {
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, bin, "debug", "models").Output()
	if err != nil {
		return nil, err
	}
	var raw codexDebugModelsOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var list []codexModelInfo
	for _, m := range raw.Models {
		if m.Visibility != "list" {
			continue
		}
		efforts := make([]string, len(m.SupportedReasoningLevels))
		for i, e := range m.SupportedReasoningLevels {
			efforts[i] = e.Effort
		}
		list = append(list, codexModelInfo{m.Slug, m.DisplayName, m.DefaultReasoningLevel, m.ContextWindow, efforts})
	}
	return list, nil
}

// codexModelsFallback is shown when `codex debug models` is unreachable; codex's known defaults as of
// today, labelled "(fallback list)" so it's never mistaken for the live catalog.
var codexModelsFallback = []codexModelInfo{
	{Slug: "gpt-6.1-sol", DisplayName: "GPT-6.1-Sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
	{Slug: "gpt-6-astra", DisplayName: "GPT-6-Astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}, DefaultEffort: "low"},
}

// codexCatalog fetches and caches `codex debug models` once per dialog; err is cached too.
func codexCatalog(d *dialog) ([]codexModelInfo, error) {
	if d.codexCatalogLoaded {
		return d.codexCatalog, d.codexCatalogErr
	}
	d.codexCatalogLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), vendorCatalogTimeout)
	defer cancel()
	d.codexCatalog, d.codexCatalogErr = codexListModelsFn(ctx)
	return d.codexCatalog, d.codexCatalogErr
}

// codexFindModel finds slug in list; nil when absent.
func codexFindModel(list []codexModelInfo, slug string) *codexModelInfo {
	for i := range list {
		if list[i].Slug == slug {
			return &list[i]
		}
	}
	return nil
}

// codexModelDesc is a catalog row's description: display name, context window, current-pick marker.
func codexModelDesc(m codexModelInfo, cur string) string {
	desc := m.DisplayName
	if m.ContextWindow > 0 {
		desc += " · " + contextLabel(m.ContextWindow)
	}
	if m.Slug == cur {
		desc += " · current"
	}
	return desc
}

// contextLabel renders a token count the way the catalog's own docs do ("500K").
func contextLabel(n int) string {
	if n >= 1000 && n%1000 == 0 {
		return strconv.Itoa(n/1000) + "K"
	}
	return strconv.Itoa(n)
}

// codexEffortChoices renders m's own supported levels as picker choices, the default level labelled.
func codexEffortChoices(m codexModelInfo) [][2]string {
	choices := make([][2]string, len(m.Efforts))
	for i, e := range m.Efforts {
		label := strings.ToUpper(e[:1]) + e[1:]
		if e != "" && e == m.DefaultEffort {
			label += " (default)"
		}
		choices[i] = [2]string{label, e}
	}
	return choices
}

// codexEffortRow pins CODEX_REASONING_EFFORT against model (cur if set, else list's first entry — codex
// models today share the same effort vocabulary); nil when model has no reasoning levels at all.
func codexEffortRow(d *dialog, list []codexModelInfo, model string) *billingRow {
	m := codexFindModel(list, model)
	if m == nil && len(list) > 0 {
		m = &list[0]
	}
	if m == nil || len(m.Efforts) == 0 {
		return nil
	}
	cur := providers.Pin(providers.PinCodexEffort)
	def := "default"
	if m.DefaultEffort != "" {
		def += " (" + m.DefaultEffort + ")"
	}
	desc := "reasoning effort on every launch " + bracket(cur != "", cur, def)
	choices := codexEffortChoices(*m)
	r := billingRow{"Effort", desc, "", func() bool {
		return pickVendorPin(d, "codex", "effort", providers.PinCodexEffort, choices, cur)
	}}
	return &r
}

// pickCodexModel is codex's Model row: Effort first (against the current pin, or list[0] when unset),
// then the live catalog (`codex debug models`), falling back to codexModelsFallback when it's unreachable.
func pickCodexModel(d *dialog) {
	start := 0
	for {
		cur, _ := providers.CodexModel()
		list, err := codexCatalog(d)
		title := "codex — model (debug models)"
		if err != nil || len(list) == 0 {
			d.printf("codex: catalog unavailable (%v); using the fallback list\n", err)
			list, title = codexModelsFallback, "codex — model (fallback list)"
		}
		var rows [][2]string
		nExtra := 0
		if r := codexEffortRow(d, list, cur); r != nil {
			rows = append(rows, [2]string{r.label, r.desc})
			nExtra = 1
			for _, m := range list {
				rows = append(rows, [2]string{m.Slug, codexModelDesc(m, cur)})
			}
			j, err := pick(d, title, rows, start)
			if err != nil || j < 0 {
				return
			}
			if j < nExtra {
				r.run()
				start = j
				continue
			}
			saveCodexModel(d, list[j-nExtra].Slug)
			clearIneligibleCodexEffort(d, list, list[j-nExtra].Slug)
			return
		}
		for _, m := range list {
			rows = append(rows, [2]string{m.Slug, codexModelDesc(m, cur)})
		}
		j, err := pick(d, title, rows, start)
		if err != nil || j < 0 {
			return
		}
		saveCodexModel(d, list[j].Slug)
		clearIneligibleCodexEffort(d, list, list[j].Slug)
		return
	}
}

// clearIneligibleCodexEffort drops the CODEX_REASONING_EFFORT pin a newly chosen model can't honor.
func clearIneligibleCodexEffort(d *dialog, list []codexModelInfo, slug string) {
	cur := providers.Pin(providers.PinCodexEffort)
	if cur == "" {
		return
	}
	m := codexFindModel(list, slug)
	if m != nil && slices.Contains(m.Efforts, cur) {
		return
	}
	if err := providers.SetPins(map[string]string{providers.PinCodexEffort: ""}); err != nil {
		d.printf("codex: %v\n", err)
		return
	}
	d.printf("codex: cleared %s pin (%s) — %s doesn't support that effort\n", providers.PinCodexEffort, cur, slug)
}

// saveCodexModel pins CODEX_MODEL.
func saveCodexModel(d *dialog, slug string) {
	if err := providers.SetPins(map[string]string{providers.PinCodexModel: slug}); err != nil {
		d.printf("model: %v\n", err)
		return
	}
	d.printf("model: codex = %s\n", slug)
	d.changed = true
}

// --- gemini: `agy models` ---------------------------------------------------------------------

// geminiEfforts are the suffixes agy's own ids embed; "xhigh"/"max" exist in --effort but rarely in ids.
var geminiEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// geminiModelInfo is one base id grouped from agy's catalog, with the efforts it offers (by id suffix).
type geminiModelInfo struct {
	Base, Name string
	Efforts    []string // "" means the base carries no effort suffix at all
}

// geminiEffortSuffix splits id into (base, effort); ok is false when id's trailing segment isn't an effort word.
func geminiEffortSuffix(id string) (base, effort string, ok bool) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return id, "", false
	}
	suffix := id[i+1:]
	if slices.Contains(geminiEfforts, suffix) {
		return id[:i], suffix, true
	}
	return id, "", false
}

// geminiListModelsFn runs `agy models` (TSV: id<TAB>display name, after a "Fetching…" banner line),
// grouping variant ids under one base entry; a seam for tests.
var geminiListModelsFn = func(ctx context.Context) ([]geminiModelInfo, error) {
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, bin, "models").Output()
	if err != nil {
		return nil, err
	}
	order := []string{}
	byBase := map[string]*geminiModelInfo{}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		id, name, ok := strings.Cut(line, "\t")
		if !ok || strings.Contains(id, "Fetching") {
			continue
		}
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" {
			continue
		}
		base, effort, hasEffort := geminiEffortSuffix(id)
		m, seen := byBase[base]
		if !seen {
			m = &geminiModelInfo{Base: base, Name: strings.TrimSuffix(name, " ("+titleCase(effort)+")")}
			byBase[base] = m
			order = append(order, base)
		}
		if hasEffort {
			m.Efforts = append(m.Efforts, effort)
		}
	}
	list := make([]geminiModelInfo, 0, len(order))
	for _, b := range order {
		list = append(list, *byBase[b])
	}
	return list, nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// geminiModelsFallback is shown when `agy models` is unreachable, labelled "(fallback list)".
var geminiModelsFallback = []geminiModelInfo{
	{Base: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Efforts: []string{"low", "medium", "high"}},
}

// geminiCatalog fetches and caches `agy models` once per dialog; err is cached too.
func geminiCatalog(d *dialog) ([]geminiModelInfo, error) {
	if d.geminiCatalogLoaded {
		return d.geminiCatalog, d.geminiCatalogErr
	}
	d.geminiCatalogLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), vendorCatalogTimeout)
	defer cancel()
	d.geminiCatalog, d.geminiCatalogErr = geminiListModelsFn(ctx)
	return d.geminiCatalog, d.geminiCatalogErr
}

// geminiFindBase finds base in list; nil when absent.
func geminiFindBase(list []geminiModelInfo, base string) *geminiModelInfo {
	for i := range list {
		if list[i].Base == base {
			return &list[i]
		}
	}
	return nil
}

// geminiCurrent is the effective (base, effort) today: the GEMINI_MODEL pin if set, else agy's own default.
func geminiCurrent() (base, effort string) {
	id := cmp.Or(providers.Pin(providers.PinGeminiModel), agent.AgyDefaultModel)
	base, effort, _ = geminiEffortSuffix(id)
	return base, effort
}

// geminiModelDesc is a catalog row's description: display name, current-pick marker.
func geminiModelDesc(m geminiModelInfo, curBase string) string {
	desc := m.Name
	if m.Base == curBase {
		desc += " · current"
	}
	return desc
}

// geminiEffortRow pins GEMINI_MODEL directly to base-effort (or base alone with no variants); nil when
// base has fewer than 2 efforts to choose between.
func geminiEffortRow(d *dialog, base, curEffort string) *billingRow {
	m := geminiFindBase(geminiCatalogOrFallback(d), base)
	if m == nil || len(m.Efforts) < 2 {
		return nil
	}
	desc := "reasoning effort on every launch " + bracket(curEffort != "", curEffort, "unset")
	r := billingRow{"Effort", desc, "", func() bool {
		rows := make([][2]string, len(m.Efforts))
		for i, e := range m.Efforts {
			rows[i] = [2]string{titleCase(e), e + curMark(e == curEffort)}
		}
		idx, err := pick(d, "gemini — effort", rows)
		if err != nil || idx < 0 {
			return false
		}
		saveGeminiModel(d, base+"-"+m.Efforts[idx])
		return true
	}}
	return &r
}

// geminiCatalogOrFallback is geminiCatalog with the fallback list substituted on error.
func geminiCatalogOrFallback(d *dialog) []geminiModelInfo {
	list, err := geminiCatalog(d)
	if err != nil || len(list) == 0 {
		return geminiModelsFallback
	}
	return list
}

// saveGeminiModel pins GEMINI_MODEL to id (a full base-effort id, or a bare base with no variants).
func saveGeminiModel(d *dialog, id string) {
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: id}); err != nil {
		d.printf("model: %v\n", err)
		return
	}
	d.printf("model: gemini = %s\n", id)
	d.changed = true
}

// pickGeminiModel is gemini's Model row: Effort first (for the current base), then one row per base
// from `agy models`, falling back to geminiModelsFallback when the catalog is unreachable.
func pickGeminiModel(d *dialog) {
	start := 0
	for {
		curBase, curEffort := geminiCurrent()
		list, err := geminiCatalog(d)
		fallback := false
		if err != nil || len(list) == 0 {
			d.printf("gemini: catalog unavailable (%v); using the fallback list\n", err)
			list, fallback = geminiModelsFallback, true
		}
		title := "gemini — model (agy models)"
		if fallback {
			title = "gemini — model (fallback list)"
		}
		var rows [][2]string
		nExtra := 0
		if r := geminiEffortRow(d, curBase, curEffort); r != nil {
			rows = append(rows, [2]string{r.label, r.desc})
			nExtra = 1
			for _, m := range list {
				rows = append(rows, [2]string{m.Base, geminiModelDesc(m, curBase)})
			}
			j, err := pick(d, title, rows, start)
			if err != nil || j < 0 {
				return
			}
			if j == 0 {
				r.run()
				start = 0
				continue
			}
			pickGeminiModelBase(d, list[j-nExtra])
			return
		}
		for _, m := range list {
			rows = append(rows, [2]string{m.Base, geminiModelDesc(m, curBase)})
		}
		j, err := pick(d, title, rows, start)
		if err != nil || j < 0 {
			return
		}
		pickGeminiModelBase(d, list[j])
		return
	}
}

// pickGeminiModelBase saves m as the pin: its base alone when it has no variants, else the current
// effort's variant when m offers it, else its first variant.
func pickGeminiModelBase(d *dialog, m geminiModelInfo) {
	if len(m.Efforts) == 0 {
		saveGeminiModel(d, m.Base)
		return
	}
	_, curEffort := geminiCurrent()
	effort := m.Efforts[0]
	if slices.Contains(m.Efforts, curEffort) {
		effort = curEffort
	}
	saveGeminiModel(d, m.Base+"-"+effort)
}
