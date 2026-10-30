package agent

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vendors"
)

func (rc routeCtx) byokCandidate(leg Leg, cand router.Candidate, tier string) Leg {
	if cand.Agent != rc.want {
		return leg
	}
	pin := rc.pins[rc.want]
	if rc.callerModel != "" && rc.callerModel != router.BYOKModel {
		return leg
	}
	if rc.callerModel == "" && pin.Model != router.BYOKModel {
		return leg
	}
	picked := cand.Profile != "" && pin.Profile == cand.Profile && pin.Model == router.BYOKModel
	return byokLeg(leg, tier, rc.reg, cand.Profile, picked)
}

// byokLeg freezes a pool choice before a runner or vendor resolver sees the request.
func byokLeg(leg Leg, tier string, reg *providers.Registry, profile string, picked bool) Leg {
	tb, err := router.Load()
	if err != nil {
		leg.Model, leg.keepCaller, leg.unroutedLeg = tier, false, false
		leg.note = "byok routing table unavailable; using tier slot"
		return leg
	}
	cell, _ := tb.Cell(leg.Agent.Name(), tier)
	p, prefix := byokProvider(leg, reg, profile)
	if p != nil && p.Preset == providers.PresetDatabricks {
		leg.repick = byokRepick(leg.Agent.Name(), tier, cell, p, prefix)
	}
	if picked && byokPoolModel(leg.Model, p) {
		return leg
	}
	model, note := router.PinnedModel(leg.Agent.Name(), tier, cell, p, router.BYOKModel)
	leg.Model, leg.note, leg.keepCaller, leg.unroutedLeg = prefix+model, note, false, false
	return leg
}

func byokProvider(leg Leg, reg *providers.Registry, profile string) (*providers.Provider, string) {
	if hub, ok := leg.Agent.(hubLeg); ok {
		return hub.provider, ""
	}
	if profile == "" {
		_, profile, _ = strings.Cut(leg.Account, "·")
	}
	p := profileRow(reg, profile)
	if p == nil {
		return nil, ""
	}
	return p, p.Name + ":"
}

func byokPoolModel(model string, p *providers.Provider) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	wire := strings.TrimPrefix(model, p.Name+":")
	return slices.ContainsFunc(providers.BYOKPool(), func(entry string) bool {
		return vendors.SpellPin(p, entry) == wire
	})
}

// byokRepick skips the failed model for this agent, then returns the next pool pick ("" if unchanged).
func byokRepick(agent, tier string, cell router.Cell, p *providers.Provider, prefix string) func(string) string {
	return func(failed string) string {
		wire := strings.TrimPrefix(failed, prefix)
		if router.SkipBYOK(agent, p, wire) == "" {
			return ""
		}
		next, _ := router.PinnedModel(agent, tier, cell, p, router.BYOKModel)
		if next == wire {
			return ""
		}
		fmt.Fprintf(os.Stderr, "byok: %s skipped for %s (output cap)\n", wire, agent)
		return prefix + next
	}
}

var capabilityErr = regexp.MustCompile(`max_new_tokens|max_output_tokens|max_tokens|context length|maximum context`)

// byokRetryModel is the next pool model when a byok leg failed on a model-capability error, else "".
func byokRetryModel(leg Leg, lreq Request, err error) string {
	if err == nil || leg.repick == nil || !capabilityErr.MatchString(err.Error()) {
		return ""
	}
	return leg.repick(lreq.Model)
}

func (leg Leg) byokNote() {
	if leg.note == "" {
		return
	}
	fmt.Fprintln(os.Stderr, leg.note)
}
