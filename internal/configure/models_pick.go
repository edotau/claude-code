// models_pick.go — an agent page's Model row: tier (with what it is for) → that account's model ids → saved.
package configure

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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

// modelRow is the agent page's Model row; nil when the agent has no account the hub can route to.
func modelRow(d *dialog, name string) *billingRow {
	if d.reg == nil {
		return nil
	}
	p, claude := modelTarget(d, name)
	if p == nil {
		return nil
	}
	desc := currentModel(d, p, claude, models.Opus) + " via " + p.Name
	return &billingRow{label: "Model", desc: desc, run: func() bool { pickModel(d, name, p, claude); return true }}
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
		idx, err := pick(d, name+" — model per tier ("+p.Name+")", rows)
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
		j, err := pick(d, name+" — "+slots[idx]+" model", choice)
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
