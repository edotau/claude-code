package agent

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

func (rc routeCtx) discoveryCandidate(disp Dispatch, cand router.Candidate, alias string) Dispatch {
	if cand.Agent != rc.want {
		return disp
	}
	pin := rc.pins[rc.want]
	if rc.callerModel != "" && rc.callerModel != router.DiscoveryModel {
		return disp
	}
	if rc.callerModel == "" && pin.Model != router.DiscoveryModel {
		return disp
	}
	picked := cand.Profile != "" && pin.Profile == cand.Profile && pin.Model == router.DiscoveryModel
	return discoveryDispatch(disp, alias, rc.reg, cand.Profile, picked)
}

// discoveryDispatch freezes a pool choice before an agent or agent resolver sees the request.
func discoveryDispatch(disp Dispatch, alias string, reg *providers.Registry, profile string, picked bool) Dispatch {
	tb, err := router.Load()
	if err != nil {
		disp.Model, disp.keepCaller, disp.unroutedDispatch = alias, false, false
		disp.note = "discovery routing table unavailable; using alias"
		return disp
	}
	cell, _ := tb.Cell(disp.Agent.Name(), alias)
	p, prefix := discoveryProvider(disp, reg, profile)
	if p != nil && p.Preset == providers.PresetDatabricks {
		disp.repick = discoveryRepick(disp.Agent.Name(), alias, cell, p, prefix)
	}
	if picked && router.IsPoolModel(p, disp.Model) {
		return disp
	}
	model, note := router.PinnedModel(disp.Agent.Name(), alias, cell, p, router.DiscoveryModel)
	disp.Model, disp.note, disp.keepCaller, disp.unroutedDispatch = prefix+model, note, false, false
	return disp
}

func discoveryProvider(disp Dispatch, reg *providers.Registry, profile string) (*providers.Provider, string) {
	if h, ok := disp.Agent.(HubEngine); ok {
		return h.HubProvider(), ""
	}
	if profile == "" {
		_, profile, _ = strings.Cut(disp.Account, "·")
	}
	p := reg.Row(profile)
	if p == nil {
		return nil, ""
	}
	return p, p.Name + ":"
}

// discoveryRepick skips the failed model for this agent, then returns the next pool pick ("" if unchanged).
func discoveryRepick(agent, alias string, cell router.Cell, p *providers.Provider, prefix string) func(string) string {
	return func(failed string) string {
		wire := strings.TrimPrefix(failed, prefix)
		if router.SkipDiscovery(agent, p, wire) == "" {
			return ""
		}
		next, _ := router.PinnedModel(agent, alias, cell, p, router.DiscoveryModel)
		if next == "" || next == wire {
			return ""
		}
		fmt.Fprintf(os.Stderr, "discovery: %s skipped for %s (model capability)\n", wire, agent)
		return prefix + next
	}
}

// capabilityErr is a model refusing the request itself: an output cap, or Google rejecting JSON-schema tool keys.
var capabilityErr = regexp.MustCompile(`max_new_tokens|max_output_tokens|max_tokens|context length|maximum context|Invalid JSON payload received\. Unknown name`)

// endpointErr is a workspace's answer for a serving endpoint that does not exist (captured from codex on Databricks).
var endpointErr = regexp.MustCompile(`ENDPOINT_NOT_FOUND`)

// servedRepickFor is a codex Databricks dispatch's repick when the switch is on and the caller named no exact model.
func (rc routeCtx) servedRepickFor(cand router.Candidate) func(string) string {
	p := rc.codexDatabricks(cand)
	if p == nil || !providers.ServedRoutingOn() || rc.exact != "" || (rc.callerModel != "" && rc.row == "") {
		return nil
	}
	return func(failed string) string { return servedRepick(p, failed) }
}

// codexDatabricks is cand's profile row when a Responses-wire agent dispatches on a Databricks profile, else nil.
func (rc routeCtx) codexDatabricks(cand router.Candidate) *providers.Provider {
	if !speaksResponses(cand.Agent) || cand.Profile == "" || rc.reg == nil {
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
	if err := router.MarkUnserved(p, wire); err != nil {
		fmt.Fprintf(os.Stderr, "served: could not mark %s unavailable: %v\n", wire, err)
	}
	next, _ := router.ServedFirst(p, true, time.Now())
	if next == "" || next == wire {
		return ""
	}
	fmt.Fprintf(os.Stderr, "served: %s unavailable on %s; using %s\n", wire, p.Name, next)
	return prefix + next
}

// retryModel is the next model after a model-capability (discovery) or endpoint-gone (served) failure, else "".
func retryModel(disp Dispatch, lreq RunOptions, err error) string {
	if err == nil {
		return ""
	}
	if retry.Progressed(err) {
		return "" // text or tools already ran: a re-run on the next model would repeat them
	}
	switch msg := err.Error(); {
	case disp.repick != nil && capabilityErr.MatchString(msg):
		return disp.repick(lreq.Model)
	case disp.servedRepick != nil && endpointErr.MatchString(msg):
		return disp.servedRepick(lreq.Model)
	}
	return ""
}

func (disp Dispatch) discoveryNote() {
	if disp.note == "" {
		return
	}
	fmt.Fprintln(os.Stderr, disp.note)
}
