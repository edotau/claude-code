// models_pick.go — an agent page's Model row: tier (with what it is for) → that account's model ids → saved.
package configure

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// tierUse says in a few words what each tier is for; a tier row stays within 80 columns.
var tierUse = map[string]string{
	models.Opus:   "deepest reasoning; hard multi-step work",
	models.Sonnet: "speed + intelligence; agentic coding",
	models.Haiku:  "fast and cheap; classify, extract, route",
	models.Fable:  "creative and long-form writing",
}

// modelTarget is the account an agent's model pick applies to; claude edits the session's slot pins instead.
func modelTarget(d *dialog, name string) (p *providers.Provider, claude bool) {
	if name == "claude" {
		sel, err := providers.Select(d.reg, "")
		if err != nil {
			return nil, true
		}
		return sel.Provider, true
	}
	p, err := agent.HubStart(d.reg, name, "")
	if err != nil {
		return nil, false
	}
	return p, false
}

// currentModel is slot's model for the agent: the resolved session slot for claude, the account's table otherwise.
func currentModel(d *dialog, p *providers.Provider, claude bool, slot string) string {
	if claude {
		if sel, err := providers.Select(d.reg, ""); err == nil {
			return sel.Slots[slot].Model
		}
		return ""
	}
	return p.Models[slot]
}

// modelRow is the agent page's Model row; nil for codex and gemini, or an agent with no account the hub can route to.
// copilot's launch argv reads only the opus slot (agent.describeCopilotRoute), so its page picks
// one model directly instead of the tier list every other agent's page shows.
func modelRow(d *dialog, name string) *billingRow {
	if d.reg == nil {
		return nil
	}
	if name == "copilot" {
		model, source := providers.CopilotModel(d.reg)
		desc := model + " (" + source + ")"
		return &billingRow{label: "Model", desc: desc, run: func() bool { pickCopilotModel(d); return true }}
	}
	if name == "codex" || name == "gemini" { // their own CLI catalogs pin CODEX_MODEL/GEMINI_MODEL: `vault edit`
		return nil
	}
	p, claude := modelTarget(d, name)
	if p == nil {
		return nil
	}
	desc := currentModel(d, p, claude, models.Opus) + " via " + p.Name
	return &billingRow{label: "Model", desc: desc, run: func() bool { pickModel(d, name, p, claude); return true }}
}

// copilotListModelsFn is the injectable seam over copilotsdk.ListModels; tests fake the SDK catalog.
var copilotListModelsFn = copilotsdk.ListModels

// copilotCatalog fetches the copilot SDK model catalog once per dialog and caches it; err is cached too,
// so a prior failure doesn't retry (and re-spawn the runtime) on every row of the same dialog.
func copilotCatalog(d *dialog) ([]copilotsdk.Model, error) {
	if d.copilotCatalogLoaded {
		return d.copilotCatalog, d.copilotCatalogErr
	}
	d.copilotCatalogLoaded = true
	bin, err := paths.VendorBin("copilot")
	if err != nil {
		d.copilotCatalogErr = err
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d.copilotCatalog, d.copilotCatalogErr = copilotListModelsFn(ctx, copilotsdk.CatalogOptions(bin, agent.CopilotToken()))
	return d.copilotCatalog, d.copilotCatalogErr
}

// copilotCatalogModel finds id in the cached catalog; ok is false when the catalog is unavailable or lacks it.
func copilotCatalogModel(d *dialog, id string) (copilotsdk.Model, bool) {
	list, err := copilotCatalog(d)
	if err != nil {
		return copilotsdk.Model{}, false
	}
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return copilotsdk.Model{}, false
}

// copilotModelDesc is a catalog row's description: name, premium multiplier, and the current-pick marker.
func copilotModelDesc(m copilotsdk.Model, cur string) string {
	desc := m.Name
	if m.Multiplier != nil {
		desc += fmt.Sprintf(" · %gx", *m.Multiplier)
	}
	if m.ID == cur {
		desc += " · current"
	}
	return desc
}

// copilotSourceFn is the injectable seam over agent.CopilotModelSource; tests fake the Databricks rows.
var copilotSourceFn = agent.CopilotModelSource

// pickCopilotModel is the copilot page's Model row: Effort and Context first (both acting on the
// current model, defaulting to "default"/"auto" = pin unset), then the model list — a Databricks
// session profile's serving endpoints, else the SDK catalog (the account copilot runs as), falling
// back to opencode's github-copilot catalog when the SDK call fails. It writes only the COPILOT_MODEL
// pin — never github-copilot's models table, which opencode's fallback ladder also reads. Picking
// Effort/Context re-shows this same picker with the cursor back on that row; picking a model saves it.
func pickCopilotModel(d *dialog) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cat, serr := copilotSourceFn(ctx, d.reg, "") // once: Effort/Context re-show the picker without refetching
	cancel()
	if serr != nil {
		d.printf("copilot: model source unavailable (%v); using the GitHub catalog\n", serr)
	}
	start := 0
	for {
		cur, _ := providers.CopilotModel(d.reg)
		title, ids := "copilot — model (SDK catalog)", []string(nil)
		var list []copilotsdk.Model
		if !cat.Databricks {
			var err error
			if list, err = copilotCatalog(d); err != nil {
				d.printf("copilot: SDK catalog unavailable (%v); using opencode's catalog instead\n", err)
				pickCopilotModelFallback(d, cur)
				return
			}
		}
		rows, extraRuns := copilotExtraRows(d, cur)
		nExtra := len(extraRuns)
		if cat.Databricks {
			title = "copilot — model (" + cat.Source + ")"
			ids, rows = appendDatabricksCopilotRows(cat, cur, rows)
		} else {
			for _, m := range list {
				if m.Enabled {
					ids = append(ids, m.ID)
					rows = append(rows, [2]string{m.ID, copilotModelDesc(m, cur)})
				}
			}
		}
		if len(ids) == 0 {
			d.printf("copilot: %s lists no usable models\n", cmp.Or(cat.Source, "SDK catalog"))
			return
		}
		j, err := pick(d, title, rows, start)
		if err != nil || j < 0 {
			return
		}
		if j < nExtra {
			extraRuns[j]()
			start = j
			continue
		}
		id := ids[j-nExtra]
		if cat.Databricks {
			saveAgentModel(d, "copilot", providers.PinCopilotModel, copilotCanonicalID(id))
			return
		}
		saveAgentModel(d, "copilot", providers.PinCopilotModel, id)
		clearIneligibleCopilotPins(d, id)
		return
	}
}

// copilotExtraRows are the Effort and Context rows shown above the model list, with their run funcs.
func copilotExtraRows(d *dialog, cur string) (rows [][2]string, runs []func() bool) {
	for _, r := range []*billingRow{copilotEffortRow(d, cur), copilotContextRow(d, cur)} {
		if r != nil {
			rows = append(rows, [2]string{r.label, r.desc})
			runs = append(runs, r.run)
		}
	}
	return rows, runs
}

// appendDatabricksCopilotRows adds one row per serving endpoint: "tested" when on the curated menu, plus current.
func appendDatabricksCopilotRows(cat agent.CopilotCatalog, cur string, rows [][2]string) ([]string, [][2]string) {
	var ids []string
	for _, r := range cat.Rows {
		var marks []string
		if r.Tested {
			marks = append(marks, "tested")
		}
		if cur != "" && copilotCanonicalID(cur) == copilotCanonicalID(r.ID) {
			marks = append(marks, "current")
		}
		ids = append(ids, r.ID)
		rows = append(rows, [2]string{r.ID, strings.Join(marks, " · ")})
	}
	return ids, rows
}

// copilotCanonicalID is id's catalog-canonical form, else id with a leading "databricks-" trimmed.
func copilotCanonicalID(id string) string {
	if c, _, ok := models.Resolve(id); ok {
		return c
	}
	return strings.TrimPrefix(id, models.DatabricksPrefix)
}

// pickCopilotModelFallback is the pre-SDK list: opencode's github-copilot catalog.
func pickCopilotModelFallback(d *dialog, cur string) {
	p := profileBy(d.reg, "github", providers.AuthCLI)
	if p == nil {
		d.printf("copilot: no github provider registered\n")
		return
	}
	ids := modelChoices(p, false)
	if len(ids) == 0 {
		d.printf("copilot: no model list available for %s\n", p.Name)
		return
	}
	choice := make([][2]string, len(ids))
	for i, id := range ids {
		choice[i] = [2]string{id, map[bool]string{true: "current", false: ""}[id == cur]}
	}
	j, err := pick(d, "copilot — model (opencode catalog)", choice)
	if err != nil || j < 0 {
		return
	}
	saveAgentModel(d, "copilot", providers.PinCopilotModel, ids[j])
}

// saveAgentModel pins an agent's model env var (copilot's COPILOT_MODEL is independent of the github-copilot opus slot).
func saveAgentModel(d *dialog, agent, pin, id string) {
	if err := providers.SetPins(map[string]string{pin: id}); err != nil {
		d.printf("model: %v\n", err)
		return
	}
	d.printf("model: %s = %s\n", agent, id)
	d.changed = true
}

// pickModel is the tier list (tier · use · current), then the account's model ids; the pick is saved at once.
func pickModel(d *dialog, name string, p *providers.Provider, claude bool) {
	slots := []string{models.Opus, models.Sonnet, models.Haiku}
	if claude {
		slots = append(slots, models.Fable)
	}
	for {
		rows := make([][2]string, len(slots))
		for i, s := range slots {
			rows[i] = [2]string{s, padTo(tierUse[s], 42) + currentModel(d, p, claude, s)}
		}
		idx, err := pick(d, name+" — model per alias ("+p.Name+")", rows)
		if err != nil || idx < 0 {
			return
		}
		ids := modelChoices(p, claude)
		if len(ids) == 0 {
			d.printf("%s: no model list available for %s\n", name, p.Name)
			return
		}
		cur := currentModel(d, p, claude, slots[idx])
		choice := make([][2]string, len(ids))
		for i, id := range ids {
			choice[i] = [2]string{id, map[bool]string{true: "current", false: ""}[id == cur]}
		}
		j, err := pick(d, name+" — "+slots[idx]+" model alias", choice)
		if err != nil || j < 0 {
			continue
		}
		saveModel(d, p, claude, slots[idx], ids[j])
	}
}

func padTo(s string, n int) string {
	if len([]rune(s)) >= n {
		return s + "  "
	}
	return s + strings.Repeat(" ", n-len([]rune(s)))
}

// saveModel writes a claude slot pin or the account's models table in the vault overlay/providers.
func saveModel(d *dialog, p *providers.Provider, claude bool, slot, id string) {
	var err error
	if claude {
		err = providers.SetPins(map[string]string{providers.SlotPin(slot): id})
	} else {
		err = providers.SetProviderModel(p.Name, slot, id)
	}
	if err != nil {
		d.printf("model: %v\n", err)
		return
	}
	if reg, err := providers.Load(); err == nil {
		d.reg = reg
		if q, ok := reg.Providers[p.Name]; ok {
			*p = *q
		}
	}
	d.printf("model: %s %s = %s\n", p.Name, slot, id)
	d.changed = true
}

// modelChoices lists ids the account can serve: opencode's catalog for a hub account, the live API (else the
// registry's own table) for claude's provider.
var modelChoices = func(p *providers.Provider, claude bool) []string {
	if !claude && p.OpencodeID != "" {
		if ids := opencodeModels(p.OpencodeID); len(ids) > 0 {
			return ids
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if ids, err := providers.LiveModels(ctx, p); err == nil && len(ids) > 0 {
		return ids
	}
	var ids []string
	for _, id := range p.Models {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// opencodeModels is `opencode models <id>` with the prefix cut; 45s because a cold catalog fetch takes 15–23s.
func opencodeModels(opencodeID string) []string {
	bin, err := paths.VendorBin("opencode")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "models", opencodeID).Output()
	var ids []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), opencodeID+"/"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}
