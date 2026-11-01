// models_menu.go — the `configure models` / "Models" row menu: one row per alias showing its current
// pin and use case, plus Unpin all; picking an alias opens the provider's live catalog and pins the choice.
package configure

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// pickModels is the models menu's picker seam; tests replace it.
var pickModels = tui.PickTable // the model table under one alias

// pickAliases is the alias menu seam; tests replace it.
var pickAliases = tui.PickBack

// modelsMenu loops the alias list + Unpin all until Back/Esc; each action re-resolves the selection
// before the next render, so a pin that just changed shows up immediately.
func modelsMenu(d *dialog) bool {
	start := 0
	for {
		sel, err := providers.Select(d.reg, "")
		if err != nil {
			d.println(err)
			return false
		}
		rows := make([][2]string, 0, len(models.AliasNames)+1)
		for _, s := range models.AliasNames {
			rows = append(rows, [2]string{s, aliasRowDetail(sel, s)})
		}
		rows = append(rows, [2]string{"Unpin all", "clear every model pin"})
		idx, err := pickAliases("models — pick a model alias to change, or Back to leave", rows, start)
		if err != nil || idx < 0 {
			return true
		}
		start = idx
		if idx == len(models.AliasNames) {
			unpinAllModels(d)
			continue
		}
		pickModelForAlias(d, sel, models.AliasNames[idx])
	}
}

// aliasRowDetail is one alias row's detail: its resolved wire model and that alias's use case (the alias
// name itself carries the family keyword UseCase matches on, so this works whatever the pinned id is).
func aliasRowDetail(sel providers.Selection, alias string) string {
	id := sel.ClientModel(alias)
	return id + " · " + models.UseCase(id, alias)
}

// pickModelForAlias lists the provider's live catalog (providers.ListModels; FallbackModels covers the
// providers whose live listing is known to fail), with a header row naming the columns, and pins the
// chosen id exactly like `claude-code models --pin <alias>=<id>`.
func pickModelForAlias(d *dialog, sel providers.Selection, alias string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, note := catalogForAlias(ctx, d.reg, sel.Provider, alias)
	if len(infos) == 0 {
		d.printf("%s: %s\n", alias, note)
		return
	}
	if note != "" {
		d.println(note)
	}
	nameW, useW := 0, 0
	for _, m := range infos {
		nameW, useW = max(nameW, len(m.Name)), max(useW, len(m.UseCase))
	}
	row := func(name, use, caps string) string {
		return strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", nameW, name, useW, use, caps), " ")
	}
	nameW, useW = max(nameW, len("Model")), max(useW, len("Use case"))
	pickRows := make([][2]string, len(infos))
	for i, m := range infos {
		pickRows[i] = [2]string{m.ID, row(m.Name, m.UseCase, m.Capabilities)}
	}
	idx, err := pickModels(alias+" — pick a model to pin", [2]string{"ID", row("Model", "Use case", "Capabilities")}, pickRows, 0)
	if err != nil || idx < 0 {
		return
	}
	pinModel(d, alias, infos[idx].ID)
}

// catalogForAlias is alias's pickable catalog: the provider's live models, or — when that listing fails
// in a known way (FallbackModels) — a known offline list, with a note explaining the substitution.
func catalogForAlias(ctx context.Context, reg *providers.Registry, p *providers.Provider, alias string) ([]providers.ModelInfo, string) {
	infos, err := providers.ListModels(ctx, reg, p)
	if err == nil {
		return infos, ""
	}
	infos, note, err := providers.FallbackModels(p, err)
	if err != nil {
		return nil, "live catalog unavailable (" + err.Error() + ") and no fallback for the " + alias + " alias"
	}
	return infos, note
}

// pinModel writes alias's pin, same call cmdModels' --pin flag makes.
func pinModel(d *dialog, alias, id string) {
	if _, err := writeAgentField(d, "claude", strings.ToUpper(alias)+"_MODEL", id); err != nil {
		d.println(err)
		return
	}
	d.printf("%-7s pinned %s\n", alias, id)
}

// unpinAllModels clears every model pin, same call cmdModels' --unpin flag makes.
func unpinAllModels(d *dialog) {
	err := agentpkg.UpdateAgentRecords(func(m map[string]agentpkg.AgentRecord) {
		r := m["claude"]
		r.Model, r.Aliases = "", nil
		m["claude"] = r
	})
	if err != nil {
		d.println(err)
		return
	}
	d.println("every model pin cleared")
}
