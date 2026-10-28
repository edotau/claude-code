// models_menu.go — the `configure models` / "Models" row menu: one row per slot showing its current
// pin and use case, plus Unpin all; picking a slot opens the provider's live catalog and pins the choice.
package configure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
)

// pickModels is the models menu's picker seam; tests replace it.
var pickModels = tui.PickTable // the model table under one slot

// pickSlots is the slot menu seam; tests replace it.
var pickSlots = tui.PickBack

// modelsMenu loops the slot list + Unpin all until Back/Esc; each action re-resolves the selection
// before the next render, so a pin that just changed shows up immediately.
func modelsMenu(d *dialog) bool {
	start := 0
	for {
		sel, err := providers.Select(d.reg, "")
		if err != nil {
			d.println(err)
			return false
		}
		rows := make([][2]string, 0, len(models.Slots)+1)
		for _, s := range models.Slots {
			rows = append(rows, [2]string{s, slotRowDetail(sel, s)})
		}
		rows = append(rows, [2]string{"Unpin all", "clear every model pin"})
		idx, err := pickSlots("models — pick a model alias to change, or Back to leave", rows, start)
		if err != nil || idx < 0 {
			return true
		}
		start = idx
		if idx == len(models.Slots) {
			unpinAllModels(d)
			continue
		}
		pickModelForSlot(d, sel, models.Slots[idx])
	}
}

// slotRowDetail is one slot row's detail: its resolved wire model and that slot's use case (the slot
// name itself carries the family keyword UseCase matches on, so this works whatever the pinned id is).
func slotRowDetail(sel providers.Selection, slot string) string {
	id := sel.ClientModel(slot)
	return id + " · " + models.UseCase(id, slot)
}

// pickModelForSlot lists the provider's live catalog (providers.ListModels; FallbackModels covers the
// providers whose live listing is known to fail), with a header row naming the columns, and pins the
// chosen id exactly like `claude-code models --pin <slot>=<id>`.
func pickModelForSlot(d *dialog, sel providers.Selection, slot string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, note := catalogForSlot(ctx, d.reg, sel.Provider, slot)
	if len(infos) == 0 {
		d.printf("%s: %s\n", slot, note)
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
	idx, err := pickModels(slot+" — pick a model to pin", [2]string{"ID", row("Model", "Use case", "Capabilities")}, pickRows, 0)
	if err != nil || idx < 0 {
		return
	}
	pinModel(d, slot, infos[idx].ID)
}

// catalogForSlot is slot's pickable catalog: the provider's live models, or — when that listing fails
// in a known way (FallbackModels) — a known offline list, with a note explaining the substitution.
func catalogForSlot(ctx context.Context, reg *providers.Registry, p *providers.Provider, slot string) ([]providers.ModelInfo, string) {
	infos, err := providers.ListModels(ctx, reg, p)
	if err == nil {
		return infos, ""
	}
	infos, note, err := providers.FallbackModels(p, err)
	if err != nil {
		return nil, "live catalog unavailable (" + err.Error() + ") and no fallback for the " + slot + " alias"
	}
	return infos, note
}

// pinModel writes slot's pin, same call cmdModels' --pin flag makes.
func pinModel(d *dialog, slot, id string) {
	if err := providers.SetPins(map[string]string{providers.SlotPin(slot): id}); err != nil {
		d.println(err)
		return
	}
	d.printf("%-7s pinned %s\n", slot, id)
}

// unpinAllModels clears every model pin, same call cmdModels' --unpin flag makes.
func unpinAllModels(d *dialog) {
	kv := map[string]string{providers.PinModel: ""}
	for _, s := range models.Slots {
		kv[providers.SlotPin(s)] = ""
	}
	if err := providers.SetPins(kv); err != nil {
		d.println(err)
		return
	}
	d.println("every model pin cleared")
}
