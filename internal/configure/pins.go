// pins.go — every agent/model pin write behind `configure`, `models --pin` and the menus; parsing is in providers/pins.go.
package configure

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// SetAgentPin sets one record field (a vault.AgentRecord.WithPin name) on agent; false when it was already that value.
func SetAgentPin(agent, field, val string) (bool, error) {
	recs, err := vault.ReadAgentRecords([]string{agent}) // a read error must not look like an empty record
	if err != nil {
		return false, err
	}
	rec := recs[agent]
	next := rec.WithPin(field, val)
	if reflect.DeepEqual(rec, next) {
		return false, nil
	}
	if field == "PROFILE" {
		if _, err := ApplyProfile([]string{agent}, val); err != nil {
			return false, err
		}
	} else if err := vault.SetAgentRecord(agent, next); err != nil {
		return false, err
	}
	return true, nil
}

// SeedProfiles lists the profiles agent can serve, its own login first; empty when reg is nil.
func SeedProfiles(reg *providers.Registry, agent string) []string {
	var own, rest []string
	if reg != nil {
		for n, p := range reg.Providers {
			switch {
			case p == nil || !agentpkg.CanServe(agent, p):
			case agent != "claude" && p.Auth.Login == agent:
				own = append(own, n)
			default:
				rest = append(rest, n)
			}
		}
	}
	slices.Sort(own)
	slices.Sort(rest)
	if agent == "opencode" {
		own = append([]string{opencode.OwnLogin}, own...)
	}
	return append(own, rest...)
}

// UnpinModels clears every claude model pin (the --unpin flag's write) and audits each.
func UnpinModels(reg *providers.Registry) error {
	set, _ := providers.ParseModelPins(reg, "", true, nil)
	return providers.ApplyClaudeModels(set)
}

// ApplyModelPins applies --pin/--unpin (non-nil preview = dry run, handed the change); handled means exit with code.
func ApplyModelPins(reg *providers.Registry, provider string, unpin bool, pins []string,
	preview func(mutate func(map[string]vault.AgentRecord)) int) (code int, handled bool) {
	if !unpin && len(pins) == 0 {
		return 0, false
	}
	set, err := providers.ParseModelPins(reg, provider, unpin, pins)
	if err != nil {
		return helptext.Fail("%v", err), true
	}
	warn, err := copilot.ValidateCopilotPins(provider, pins)
	if err != nil {
		return helptext.Fail("%v", err), true
	}
	if warn != "" {
		fmt.Fprintln(os.Stderr, "claude-code: models:", warn)
	}
	if preview != nil {
		return preview(func(m map[string]vault.AgentRecord) { m["claude"] = providers.WithClaudeModels(m["claude"], set) }), true
	}
	if err := providers.ApplyClaudeModels(set); err != nil {
		return helptext.Fail("%v", err), true
	}
	return 0, false
}

// CatalogForAlias lists live choices plus saved additions, with an explained offline fallback.
func CatalogForAlias(ctx context.Context, reg *providers.Registry, p *providers.Provider, alias string) (choices []providers.ModelInfo, note string) {
	defer func() {
		if p.Provider == "github" {
			return // Saved history must not bypass Copilot policy filtering.
		}
		saved, err := providers.PickerModels(p)
		if err != nil {
			note += " saved picker additions unavailable: " + err.Error()
			return
		}
		choices = providers.MergePickerModels(choices, saved)
	}()
	var infos []providers.ModelInfo
	var err error
	if p.Provider == "github" {
		infos, err = copilot.ListModels(ctx)
	} else {
		infos, err = providers.ListModels(ctx, p)
	}
	if err == nil {
		return infos, ""
	}
	infos, note, err = providers.FallbackModels(p, err)
	if err != nil {
		return nil, "live catalog unavailable (" + err.Error() + ") and no fallback for the " + alias + " alias"
	}
	return infos, note
}
