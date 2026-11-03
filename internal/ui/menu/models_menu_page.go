package menu

import (
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
)

// catalogFn is the discovery seam over catalogForAlias; tests stub the served-model listing.
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
	d.shell().Run("models ("+profile+") — pick an alias, or Back to leave", func() []uidialog.Row {
		if cur, err := providers.Select(d.reg, profile); err == nil {
			sel = cur
		}
		var rows []uidialog.Row
		for _, s := range models.AliasNames {
			rows = append(rows, uidialog.Row{Name: s, Detail: aliasRowDetail(sel, s), Run: func() bool {
				pickModelForAlias(d, sel, s)
				return false
			}})
		}
		if withUnpin {
			rows = append(rows, uidialog.Row{Name: "Unpin all", Detail: "clear every model pin", Run: func() bool {
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
	r := []rune(strings.Join(parts, " · "))
	if len(r) > 52 {
		r = append(r[:51], '…')
	}
	return string(r)
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
	spec := agentconfig.QualifyPinSpec(d.reg, profile, id)
	if _, err := writeAgentField(d, "claude", strings.ToUpper(alias)+"_MODEL", spec); err != nil {
		d.println(err)
		return
	}
	providers.AuditModelPin(alias, spec)
	d.printf("%-7s pinned %s\n", alias, spec)
}

// unpinAllModels clears every model pin, same call cmdModels' --unpin flag makes, and audits each.
func unpinAllModels(d *dialog) {
	set, _ := agentconfig.ParseModelPins(d.reg, "", true, nil)
	if err := agentconfig.ApplyClaudeModels(set); err != nil {
		d.println(err)
		return
	}
	d.println("every model pin cleared")
}
