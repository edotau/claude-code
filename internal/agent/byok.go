package agent

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

func (rc routeCtx) byokCandidate(leg Leg, cand Candidate, tier string) Leg {
	if cand.Agent != rc.want {
		return leg
	}
	pin := rc.pins[rc.want]
	if rc.callerModel != "" && rc.callerModel != BYOKModel {
		return leg
	}
	if rc.callerModel == "" && pin.Model != BYOKModel {
		return leg
	}
	picked := cand.Profile != "" && pin.Profile == cand.Profile && pin.Model == BYOKModel
	return byokLeg(leg, tier, rc.reg, cand.Profile, picked)
}

// byokLeg freezes a pool choice before a runner or agent resolver sees the request.
func byokLeg(leg Leg, tier string, reg *providers.Registry, profile string, picked bool) Leg {
	tb, err := Load()
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
	model, note := PinnedModel(leg.Agent.Name(), tier, cell, p, BYOKModel)
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
	return IsPoolModel(p, model)
}

// byokRepick skips the failed model for this agent, then returns the next pool pick ("" if unchanged).
func byokRepick(agent, tier string, cell Cell, p *providers.Provider, prefix string) func(string) string {
	return func(failed string) string {
		wire := strings.TrimPrefix(failed, prefix)
		if SkipBYOK(agent, p, wire) == "" {
			return ""
		}
		next, _ := PinnedModel(agent, tier, cell, p, BYOKModel)
		if next == "" || next == wire {
			return ""
		}
		fmt.Fprintf(os.Stderr, "byok: %s skipped for %s (model capability)\n", wire, agent)
		return prefix + next
	}
}

// capabilityErr is a model refusing the request itself: an output cap, or Google rejecting JSON-schema tool keys.
var capabilityErr = regexp.MustCompile(`max_new_tokens|max_output_tokens|max_tokens|context length|maximum context|Invalid JSON payload received\. Unknown name`)

// endpointErr is a workspace's answer for a serving endpoint that does not exist (captured from codex on Databricks).
var endpointErr = regexp.MustCompile(`ENDPOINT_NOT_FOUND`)

// servedRepickFor is a codex Databricks leg's repick when the switch is on and the caller named no exact model.
func (rc routeCtx) servedRepickFor(cand Candidate) func(string) string {
	p := rc.codexDatabricks(cand)
	if p == nil || !providers.ServedRoutingOn() || rc.exact != "" || (rc.callerModel != "" && rc.row == "") {
		return nil
	}
	return func(failed string) string { return servedRepick(p, failed) }
}

// codexDatabricks is cand's profile row when cand is a codex leg on a Databricks profile, else nil.
func (rc routeCtx) codexDatabricks(cand Candidate) *providers.Provider {
	if cand.Agent != "codex" || cand.Profile == "" || rc.reg == nil {
		return nil
	}
	if p := rc.reg.Providers[cand.Profile]; p != nil && p.Preset == providers.PresetDatabricks {
		return p
	}
	return nil
}

// servedRepick marks the failed model gone on p and returns the next READY Responses model, "" when there is none.
func servedRepick(p *providers.Provider, failed string) string {
	prefix := p.Name + ":"
	wire := strings.TrimPrefix(failed, prefix)
	if err := MarkUnserved(p, wire); err != nil {
		fmt.Fprintf(os.Stderr, "served: could not mark %s unavailable: %v\n", wire, err)
	}
	next, _ := ServedFirst(p, true, time.Now())
	if next == "" || next == wire {
		return ""
	}
	fmt.Fprintf(os.Stderr, "served: %s unavailable on %s; using %s\n", wire, p.Name, next)
	return prefix + next
}

// retryModel is the next model after a model-capability (byok) or endpoint-gone (served) failure, else "".
func retryModel(leg Leg, lreq RunOptions, err error) string {
	if err == nil {
		return ""
	}
	switch msg := err.Error(); {
	case leg.repick != nil && capabilityErr.MatchString(msg):
		return leg.repick(lreq.Model)
	case leg.servedRepick != nil && endpointErr.MatchString(msg):
		return leg.servedRepick(lreq.Model)
	}
	return ""
}

func (leg Leg) byokNote() {
	if leg.note == "" {
		return
	}
	fmt.Fprintln(os.Stderr, leg.note)
}
