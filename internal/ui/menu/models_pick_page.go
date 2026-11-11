package menu

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// aliasUse says in a few words what each alias is for; an alias row stays within 80 columns.
var aliasUse = map[string]string{
	models.Opus:   "deepest reasoning; hard multi-step work",
	models.Sonnet: "speed + intelligence; agentic coding",
	models.Haiku:  "fast and cheap; classify, extract, route",
	models.Fable:  "creative and long-form writing",
}

// modelTarget is the account an agent's model pick applies to; claude edits the session's alias pins instead.
func modelTarget(d *dialog, name string) (p *providers.Provider, claude bool) {
	if name == "claude" {
		sel, err := providers.Select(d.reg, "")
		if err != nil {
			return nil, true
		}
		return sel.Provider, true
	}
	if p := d.reg.Providers[vault.GetAgentRecord(name).Profile]; p != nil && p.Protocol != "" {
		return p, false // a pinned registry profile (e.g. Databricks dev) owns the pick, not the hub ladder
	}
	p, err := opencode.HubStart(d.reg, name, "")
	if err != nil {
		return nil, false
	}
	return p, false
}

// currentModel is alias's model for the agent: the resolved session alias for claude, the account's table otherwise.
func currentModel(d *dialog, p *providers.Provider, claude bool, alias string) string {
	if claude {
		if sel, err := providers.Select(d.reg, ""); err == nil {
			return sel.Aliases[alias].Model
		}
		return ""
	}
	return p.Models[alias]
}

// modelRow is the agent page's Model row; nil for codex and gemini, or an agent with no account the hub can route to.
// copilot's launch argv reads only the opus alias (agent.describeCopilotRoute), so its page picks
// one model directly instead of the alias list every other agent's page shows.
func modelRow(d *dialog, name string) *billingRow {
	if d.reg == nil {
		return nil
	}
	if name == "copilot" {
		desc := copilotRouteDesc(d)
		return &billingRow{label: "Model", desc: desc, run: func() bool { pickCopilotModel(d); return true }}
	}
	if name == "codex" || name == "gemini" { // their own CLI catalogs pin CODEX_MODEL/GEMINI_MODEL: `vault edit`
		return nil
	}
	p, claude := modelTarget(d, name)
	if p == nil {
		return nil
	}
	cur := currentModel(d, p, claude, models.Opus)
	desc := modelDesc(p.Name, cur, modelListed(p, claude, cur))
	return &billingRow{label: "Model", desc: desc, run: func() bool { pickModel(d, name, p, claude); return true }}
}

// modelDesc is the Model row's detail: the provider, then the active model [alias or id], green when listed.
func modelDesc(provider, model string, listed bool) string {
	if model == "" {
		return provider
	}
	return provider + " " + checked(listed, cmp.Or(models.AliasOf(model), model)) // the picker aligns the badge column
}

// modelListed: model is in the agent's static catalog (claude: the model catalog; others: the opencode menu or the
// account's own table); no network.
func modelListed(p *providers.Provider, claude bool, model string) bool {
	if _, _, ok := models.Resolve(model); ok && claude {
		return true
	}
	if slices.ContainsFunc(providers.OpencodeMenu(), func(m providers.OpencodeModel) bool { return m.ID == model }) {
		return true
	}
	for _, id := range p.Models {
		if id == model {
			return true
		}
	}
	return false
}

// copilotRouteDesc is copilot's Model row detail: its route, then the pinned (else default) model. Listed means the
// sealed Databricks snapshot or this dialog's already-fetched catalog holds it; it never fetches.
func copilotRouteDesc(d *dialog) string {
	model, _ := providers.CopilotModel(d.reg)
	name := cmp.Or(vault.GetAgentRecord("copilot").Profile, providers.SessionProfile())
	if p := d.reg.Providers[name]; p != nil && p.Preset == providers.PresetDatabricks {
		return modelDesc("databricks-"+name, model, snapshotHas(p, model))
	}
	listed := false
	if d.copilotCatalogLoaded && d.copilotCatalogErr == nil {
		_, listed = copilotCatalogModel(d, model)
	}
	return modelDesc("github-copilot", model, listed)
}

// snapshotHas reads p's sealed discovery snapshot (vault only, no network, no refresh) for model.
func snapshotHas(p *providers.Provider, model string) bool {
	subject, found, err := serving.RecallSubject(p)
	if err != nil || !found {
		return false
	}
	scope, err := serving.NewScope(p, subject)
	if err != nil {
		return false
	}
	snap, found, err := serving.Read(scope, true)
	if err != nil || !found {
		return false
	}
	want := copilotCanonicalID(model)
	return slices.ContainsFunc(snap.Models, func(m providers.ModelInfo) bool { return copilotCanonicalID(m.ID) == want })
}

// copilotCatalog fetches the copilot SDK model catalog once per dialog and caches it; err is cached too,
// so a prior failure doesn't retry (and re-spawn the runtime) on every row of the same dialog.
// copilotCatalogFn is the catalog source (tests stub it so no copilot runtime is spawned).
var copilotCatalogFn = copilot.Catalog

func copilotCatalog(d *dialog) ([]copilot.SDKModel, error) {
	if d.copilotCatalogLoaded {
		return d.copilotCatalog, d.copilotCatalogErr
	}
	d.copilotCatalogLoaded = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d.copilotCatalog, d.copilotCatalogErr = copilotCatalogFn(ctx)
	return d.copilotCatalog, d.copilotCatalogErr
}

// copilotCatalogModel finds id in the cached catalog; ok is false when the catalog is unavailable or lacks it.
func copilotCatalogModel(d *dialog, id string) (copilot.SDKModel, bool) {
	list, err := copilotCatalog(d)
	if err != nil {
		return copilot.SDKModel{}, false
	}
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return copilot.SDKModel{}, false
}

// copilotModelDesc is a catalog row's description: name, premium multiplier, and the current-pick marker.
func copilotModelDesc(m copilot.SDKModel, cur string) string {
	desc := m.Name
	if m.Multiplier != nil {
		desc += fmt.Sprintf(" · %gx", *m.Multiplier)
	}
	if m.ID == cur {
		desc += " · current"
	}
	return desc
}

// modelTitle is a model picker's title: "<what> (<provider> · <profile>)".
func modelTitle(what, provider, profile string) string {
	return what + " (" + provider + " · " + profile + ")"
}

// copilotSourceFn is the injectable seam over copilot.CopilotModelSource; tests fake the Databricks rows.
var copilotSourceFn = copilot.CopilotModelSource

// pickCopilotModel is the copilot page's Model row: Effort and Context first (both acting on the
// current model, defaulting to "default"/"auto" = pin unset), then the model list — a Databricks
// session profile's serving endpoints, else the SDK catalog (the account copilot runs as), falling
// back to opencode's github-copilot catalog when the SDK call fails. It writes only the COPILOT_MODEL
// pin — never github-copilot's models table, which opencode's fallback ladder also reads. Picking
// Effort/Context re-shows this same picker with the cursor back on that row; picking a model saves it.
func pickCopilotModel(d *dialog) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cat, serr := copilotSourceFn(ctx, d.reg, vault.GetAgentRecord("copilot").Profile) // once: Effort/Context re-show the picker without refetching
	cancel()
	if serr != nil {
		d.printf("copilot: model source unavailable (%v); using the GitHub catalog\n", serr)
	}
	start := 0
	for {
		cur, _ := providers.CopilotModel(d.reg)
		pk, err := copilotPickRows(d, cat, cur)
		if err != nil {
			d.printf("copilot: SDK catalog unavailable (%v); using opencode's catalog instead\n", err)
			pickCopilotModelFallback(d, cur)
			return
		}
		if len(pk.ids) == 0 {
			d.printf("copilot: %s lists no usable models\n", cmp.Or(cat.Source, "SDK catalog"))
			return
		}
		j, err := d.shell().Pick(pk.title, pk.rows, start)
		if err != nil || j < 0 {
			return
		}
		if j < len(pk.extra) {
			pk.extra[j]()
			start = j
			continue
		}
		id := pk.ids[j-len(pk.extra)]
		if cat.Databricks {
			saveAgentModel(d, "copilot", copilotCanonicalID(id))
			return
		}
		if saveAgentModel(d, "copilot", id) {
			clearIneligibleCopilotPins(d, id)
		}
		return
	}
}

// copilotPick is one rendering of the copilot Model picker: rows lead with the extra (Effort/Context) rows, then ids.
type copilotPick struct {
	title string
	ids   []string
	rows  [][2]string
	extra []func() bool
}

// copilotPickRows builds the picker for cat (a Databricks profile's endpoints, else the SDK catalog's enabled models);
// the error is the SDK catalog being unavailable.
func copilotPickRows(d *dialog, cat copilot.CopilotCatalog, cur string) (copilotPick, error) {
	pk := copilotPick{title: modelTitle("copilot — model", "github", "copilot catalog")}
	var list []copilot.SDKModel
	if !cat.Databricks {
		var err error
		if list, err = copilotCatalog(d); err != nil {
			return pk, err
		}
	}
	pk.rows, pk.extra = copilotExtraRows(d, cur)
	if cat.Databricks {
		pk.title = modelTitle("copilot — model", "databricks", cat.Profile)
		pk.ids, pk.rows = appendDatabricksCopilotRows(cat, cur, pk.rows)
		return pk, nil
	}
	for _, m := range list {
		if m.Enabled {
			pk.ids = append(pk.ids, m.ID)
			pk.rows = append(pk.rows, [2]string{m.ID, copilotModelDesc(m, cur)})
		}
	}
	return pk, nil
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
func appendDatabricksCopilotRows(cat copilot.CopilotCatalog, cur string, rows [][2]string) ([]string, [][2]string) {
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
	p := d.reg.ProfileBy("github", providers.AuthCLI)
	if p == nil {
		d.printf("copilot: no github provider registered\n")
		return
	}
	ids := pickerChoices(d, p, false)
	if len(ids) == 0 {
		d.printf("copilot: no model list available for %s\n", p.Name)
		return
	}
	j, err := d.shell().Pick(modelTitle("copilot — model", "github", p.Name), idRows(ids, cur))
	if err != nil || j < 0 {
		return
	}
	saveAgentModel(d, "copilot", ids[j])
}

// idRows is one picker row per model id, the one equal to cur marked "current".
func idRows(ids []string, cur string) [][2]string {
	rows := make([][2]string, len(ids))
	for i, id := range ids {
		rows[i] = [2]string{id, map[bool]string{true: "current", false: ""}[id == cur]}
	}
	return rows
}

// saveAgentModel stores an agent's model in its record (copilot's is independent of the github-copilot opus alias).
func saveAgentModel(d *dialog, agent, id string) bool {
	if _, err := writeAgentField(d, agent, "MODEL", id); err != nil {
		d.printf("model: %v\n", err)
		return false
	}
	d.printf("model: %s = %s\n", agent, id)
	return true
}

// pickModel is the alias list (alias · use · current), then the account's model ids; the pick is saved at once.
func pickModel(d *dialog, name string, p *providers.Provider, claude bool) {
	aliases := []string{models.Opus, models.Sonnet, models.Haiku}
	if claude {
		aliases = append(aliases, models.Fable)
	}
	choices := sync.OnceValue(func() []string { return pickerChoices(d, p, claude) }) // alias-independent; a fetch takes 20–45s
	for {
		rows := make([][2]string, len(aliases))
		for i, s := range aliases {
			rows[i] = [2]string{s, padTo(aliasUse[s], 42) + currentModel(d, p, claude, s)}
		}
		idx, err := d.shell().Pick(modelTitle(name+" — model per alias", p.Provider, p.Name), rows)
		if err != nil || idx < 0 {
			return
		}
		ids := choices()
		if len(ids) == 0 {
			d.printf("%s: no model list available for %s\n", name, p.Name)
			return
		}
		cur := currentModel(d, p, claude, aliases[idx])
		j, err := d.shell().Pick(modelTitle(name+" — "+aliases[idx]+" model alias", p.Provider, p.Name), idRows(ids, cur))
		if err != nil || j < 0 {
			continue
		}
		saveModel(d, p, claude, aliases[idx], ids[j])
	}
}

// saveModel writes a claude alias pin or the account's models table in the vault overlay/providers.
func saveModel(d *dialog, p *providers.Provider, claude bool, alias, id string) {
	var err error
	switch {
	case claude:
		_, err = writeAgentField(d, "claude", strings.ToUpper(alias)+"_MODEL", id)
	case p.Protocol == "":
		d.printf("model: %s is opencode's own plan; pin a profile to save a model\n", p.Name)
		return
	default:
		err = providers.SetProviderModel(p.Name, alias, id)
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
	d.printf("model: %s %s = %s\n", p.Name, alias, id)
	d.changed = true
}

// pickerChoices is modelChoices plus the account's saved models; a failed saved-model read is printed on d.
func pickerChoices(d *dialog, p *providers.Provider, claude bool) []string {
	choices := modelChoices(p, claude)
	if p.Provider == "github" { // Copilot keeps its live enabled catalog, not saved history.
		return choices
	}
	saved, err := providers.PickerModels(p)
	if err != nil {
		d.printf("models: %v\n", err)
		return choices
	}
	for _, m := range saved {
		if !slices.Contains(choices, m.ID) {
			choices = append(choices, m.ID)
		}
	}
	return choices
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
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "models", opencodeID).Output()
	if err != nil {
		return nil // a timeout or crash falls through to the caller's live/static catalog
	}
	var ids []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), opencodeID+"/"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// catalogFn is the discovery seam over configure.CatalogForAlias; tests stub the served-model listing.
var catalogFn = configure.CatalogForAlias

// modelsMenu is the session profile's alias page (aliases + Unpin all) until Back/Esc.
func modelsMenu(d *dialog) bool {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		d.println(err)
		return false
	}
	modelsPage(d, sel, true)
	return true
}

// offerModels is the profile/login flow's discovery step: the profile's served models, picked per alias.
func offerModels(d *dialog, profile string) {
	sel, err := providers.Select(d.reg, profile)
	if err != nil {
		d.println(err)
		return
	}
	modelsPage(d, sel, false)
}

// modelsPage loops the alias rows (plus Unpin all when withUnpin); each pass re-resolves the selection so a
// fresh pin shows at once.
func modelsPage(d *dialog, sel providers.Selection, withUnpin bool) {
	profile := sel.Provider.Name
	d.shell().Run("models ("+profile+") — pick an alias, or Back to leave", func() []shellRow {
		if cur, err := providers.Select(d.reg, profile); err == nil {
			sel = cur
		}
		var rows []shellRow
		for _, s := range models.AliasNames {
			rows = append(rows, shellRow{Name: s, Detail: aliasRowDetail(sel, s), Run: func() bool {
				pickModelForAlias(d, sel, s)
				return false
			}})
		}
		if withUnpin {
			rows = append(rows, shellRow{Name: "Unpin all", Detail: "clear every model pin", Run: func() bool {
				unpinAllModels(d)
				return false
			}})
		}
		return rows
	})
}

// aliasRowDetail is one alias row's detail: its resolved wire model and that alias's use case (the alias
// name itself carries the family keyword UseCase matches on, so this works whatever the pinned id is).
func aliasRowDetail(sel providers.Selection, alias string) string {
	id := sel.ClientModel(alias)
	return id + " · " + models.UseCase(id, alias)
}

// modelRowDetail is a catalog row's one-line detail: name, use case, capabilities, cut to fit 80 columns.
func modelRowDetail(m providers.ModelInfo) string {
	var parts []string
	for _, p := range []string{m.Name, m.UseCase, m.Capabilities} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return textutil.Truncate(strings.Join(parts, " · "), 52)
}

// pickModelForAlias lists the profile's served models (providers.ListModels; FallbackModels covers the
// providers whose live listing is known to fail) and pins the choice like `claude-code models --pin <alias>=<id>`.
func pickModelForAlias(d *dialog, sel providers.Selection, alias string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, note := catalogFn(ctx, d.reg, sel.Provider, alias)
	if len(infos) == 0 {
		d.printf("%s: %s\n", alias, note)
		return
	}
	if note != "" {
		d.println(note)
	}
	rows := make([][2]string, len(infos))
	for i, m := range infos {
		rows[i] = [2]string{m.ID, modelRowDetail(m)}
	}
	idx, err := d.shell().Pick(alias+" — pick a model to pin", rows)
	if err != nil || idx < 0 {
		return
	}
	pinModel(d, sel.Provider.Name, alias, infos[idx].ID)
}

// pinModel writes alias's audited pin, qualified to profile when it is not the session's own (as --pin does).
func pinModel(d *dialog, profile, alias, id string) {
	spec := providers.QualifyPinSpec(d.reg, profile, id)
	if _, err := writeAgentField(d, "claude", strings.ToUpper(alias)+"_MODEL", spec); err != nil {
		d.println(err)
		return
	}
	providers.AuditModelPin(alias, spec)
	d.printf("%-7s pinned %s\n", alias, spec)
}

// unpinAllModels clears every model pin, same call cmdModels' --unpin flag makes, and audits each.
func unpinAllModels(d *dialog) {
	if err := configure.UnpinModels(d.reg); err != nil {
		d.println(err)
		return
	}
	d.changed = true
	d.println("every model pin cleared")
}
