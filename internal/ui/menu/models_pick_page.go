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
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// aliasUse says in a few words what each alias is for; an alias row stays within 80 columns.
var aliasUse = map[string]string{
	models.Opus:   "deepest reasoning; multi-step work",
	models.Sonnet: "balanced speed; agentic coding",
	models.Haiku:  "fast and cheap; classify, route",
	models.Fable:  "demanding reasoning; long-horizon",
}

// modelTarget is the account a model pick applies to; claude edits the session's alias pins instead.
func modelTarget(d *dialog, name string) (p *providers.Provider, claude bool) {
	if name == "claude" {
		sel, err := providers.Select(d.reg, "")
		if err != nil {
			return nil, true
		}
		return sel.Provider, true
	}
	if p := d.reg.Providers[vault.GetAgentRecord(name).Profile]; p != nil && p.Protocol != "" {
		return p, false
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

// modelRow is the agent page's Model row; copilot reads only the opus alias, so it picks one model directly.
func modelRow(d *dialog, name string) *billingRow {
	if d.reg == nil {
		return nil
	}
	if name == "copilot" {
		desc := copilotRouteDesc(d)
		return &billingRow{label: "Model", desc: desc, run: func() bool { pickCopilotModel(d); return true }}
	}
	if name == "codex" || name == "gemini" {
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
	canonical, _, _ := models.Resolve(model)
	return provider + " " + checked(listed, textutil.Truncate(cmp.Or(models.AliasOf(canonical), model), 20))
}

// modelListed reports whether model is in the agent's static catalog, with no network.
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

// copilotRouteDesc is copilot's Model row detail: its route, then the pinned (else default) model.
func copilotRouteDesc(d *dialog) string {
	model, _ := providers.CopilotModel(d.reg)
	name := cmp.Or(vault.GetAgentRecord("copilot").Profile, providers.SessionProfileContext(context.Background()))
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
	subject, found, err := serving.RecallSubjectCtx(context.Background(), p)
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

var copilotCatalogFn = copilot.Catalog

// copilotCatalog reads the model catalog once per dialog and caches the error too, so a failure never re-spawns it.
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
		if models.SameModel(m.ID, id) {
			return m, true
		}
	}
	return copilot.SDKModel{}, false
}

func copilotModelDesc(m copilot.SDKModel, cur string) string {
	desc := m.Name
	if m.Multiplier != nil {
		desc += fmt.Sprintf(" · %gx", *m.Multiplier)
	}
	if models.SameModel(m.ID, cur) {
		desc += " · current"
	}
	return desc
}

func modelTitle(what, provider, profile string) string {
	return what + " (" + provider + " · " + profile + ")"
}

// copilotSourceFn is the test seam over copilot.CopilotModelSource.
var copilotSourceFn = copilot.CopilotModelSource

// pickCopilotModel pins copilot's model and clears effort/context pins it cannot honor; never github-copilot's models.
func pickCopilotModel(d *dialog) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cat, serr := copilotSourceFn(ctx, d.reg, vault.GetAgentRecord("copilot").Profile)
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
		saveCopilotPick(d, cat, pk.ids[j-len(pk.extra)])
		return
	}
}

// saveCopilotPick pins id as copilot's model, then drops the effort/context pins the new model cannot honor.
func saveCopilotPick(d *dialog, cat copilot.CopilotCatalog, id string) {
	if cat.Databricks {
		id = copilotCanonicalID(id)
		if saveAgentModel(d, "copilot", id) {
			withoutSDKCatalog(d, func() { clearIneligibleCopilotPins(d, id) })
		}
		return
	}
	if saveAgentModel(d, "copilot", id) {
		clearIneligibleCopilotPins(d, id)
	}
}

// copilotPick is one rendering of the copilot Model picker: rows lead with the extra (Effort/Context) rows, then ids.
type copilotPick struct {
	title string
	ids   []string
	rows  [][2]string
	extra []func() bool
}

// copilotPickRows builds the picker for cat.
func copilotPickRows(d *dialog, cat copilot.CopilotCatalog, cur string) (copilotPick, error) {
	pk := copilotPick{title: modelTitle("copilot — model", "github", "copilot catalog")}
	if cat.Databricks {
		pk.title = modelTitle("copilot — model", "databricks", cat.Profile)
		pk.rows, pk.extra = copilotExtraRowsNoSDK(d, cur)
		pk.ids, pk.rows = appendDatabricksCopilotRows(cat, cur, pk.rows)
		return pk, nil
	}
	list, err := copilotCatalog(d)
	if err != nil {
		return pk, err
	}
	pk.rows, pk.extra = copilotExtraRows(d, cur)
	for _, m := range list {
		if m.Enabled {
			pk.ids = append(pk.ids, m.ID)
			pk.rows = append(pk.rows, [2]string{m.ID, copilotModelDesc(m, cur)})
		}
	}
	return pk, nil
}

// copilotExtraRowsNoSDK is copilotExtraRows for Databricks endpoints: no SDK catalog fetch, cache left untouched.
func copilotExtraRowsNoSDK(d *dialog, cur string) (rows [][2]string, runs []func() bool) {
	withoutSDKCatalog(d, func() { rows, runs = copilotExtraRows(d, cur) })
	return rows, runs
}

// withoutSDKCatalog runs fn with the dialog's SDK catalog cache emptied (no fetch) and restores it after.
func withoutSDKCatalog(d *dialog, fn func()) {
	loaded, list, err := d.copilotCatalogLoaded, d.copilotCatalog, d.copilotCatalogErr
	d.copilotCatalogLoaded, d.copilotCatalog, d.copilotCatalogErr = true, nil, nil
	defer func() { d.copilotCatalogLoaded, d.copilotCatalog, d.copilotCatalogErr = loaded, list, err }()
	fn()
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

// appendDatabricksCopilotRows adds one row per serving endpoint: "verified" when on the curated menu, plus current.
func appendDatabricksCopilotRows(cat copilot.CopilotCatalog, cur string, rows [][2]string) ([]string, [][2]string) {
	var ids []string
	for _, r := range cat.Rows {
		var marks []string
		if r.Tested {
			marks = append(marks, "verified")
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
	choices := sync.OnceValue(func() []string { return pickerChoices(d, p, claude) })
	for {
		rows := make([][2]string, len(aliases))
		for i, s := range aliases {
			rows[i] = [2]string{s, padTo(aliasUse[s], 36) + currentModel(d, p, claude, s)}
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
	if p.Provider == "github" {
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

// modelChoices lists ids the account can serve.
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
	cmd := exec.CommandContext(ctx, bin, "models", opencodeID)
	cmd.WaitDelay = proc.ExecWaitDelay
	out, err := proc.OutputCapped(cmd)
	if err != nil {
		// a timeout or crash falls through to the caller's live/static catalog
		return nil
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

// modelsPage loops the alias rows.
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

// aliasRowDetail is one alias row's detail: its resolved wire model and the use case of that id (General when unknown).
func aliasRowDetail(sel providers.Selection, alias string) string {
	id := sel.ClientModel(alias)
	return id + " · " + models.UseCase(id, alias)
}

// modelRowDetail is a catalog row's one-line detail: name, use case, capabilities, cut to width columns.
func modelRowDetail(m providers.ModelInfo, width int) string {
	var parts []string
	for _, p := range []string{m.Name, m.UseCase, m.Capabilities} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return textutil.Truncate(strings.Join(parts, " · "), width)
}

// pickModelForAlias lists the profile's served models.
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
	idW := 0
	for _, m := range infos {
		idW = max(idW, len(m.ID))
	}
	rows := make([][2]string, len(infos))
	for i, m := range infos {
		rows[i] = [2]string{m.ID, modelRowDetail(m, max(78-2-idW, 20))}
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
