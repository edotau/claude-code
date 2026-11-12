package agent

import (
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Finding is one failed preflight check on a Databricks target; Check is credential, auth, served or wire.
type Finding struct {
	Check   string `json:"check"`
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
	Detail  string `json:"detail"`
}

func (f Finding) String() string { return f.Check + ": " + f.Detail }

// Preflight is what a dispatch's preflight found and chose; the record and the preflight: lines carry it.
type Preflight struct {
	Requested string    `json:"requested,omitempty"` // <profile>[:<model>] as asked, set by the first failed check
	Chosen    string    `json:"chosen,omitempty"`    // the dispatch the chain starts on
	Failed    []Finding `json:"failed,omitempty"`
	Notes     []string  `json:"notes,omitempty"` // checks skipped or soft: a stale listing
}

// fail records a failed check; the first one names what was requested.
func (pf *Preflight) fail(f Finding) {
	if pf == nil {
		return
	}
	if pf.Requested == "" {
		pf.Requested = strings.TrimSuffix(f.Profile+":"+f.Model, ":")
	}
	pf.Failed = append(pf.Failed, f)
}

// note records a skipped or soft check once.
func (pf *Preflight) note(s string) {
	if pf == nil || s == "" || slices.Contains(pf.Notes, s) {
		return
	}
	pf.Notes = append(pf.Notes, s)
}

// Lines is one line per failed check, then one per note; nil when the preflight found nothing.
func (pf *Preflight) Lines() []string {
	if pf == nil {
		return nil
	}
	var out []string
	for _, f := range pf.Failed {
		chosen := pf.Chosen
		if chosen == "" {
			chosen = "nothing"
		}
		out = append(out, pf.Requested+" failed "+f.Check+" ("+f.Detail+"); chose "+chosen)
	}
	for _, n := range pf.Notes {
		out = append(out, "note: "+n)
	}
	return out
}

// PreflightError is a target the preflight refused with fallback off.
type PreflightError struct{ Finding Finding }

func (e *PreflightError) Error() string {
	return "preflight: " + e.Finding.String() + " (fallback is off: --fallback or " + router.EnvFallback + ")"
}

// refreshListing starts one detached re-list of a workspace's listing, deduped per profile; a seam for tests.
var refreshListing = func(profile string) { catalog.SpawnDiscover(profile, false) }

// CheckWorkspace is the local, no-network check of a Databricks row: a stored credential and no live auth mark.
func CheckWorkspace(p *providers.Provider, st router.State, now time.Time) []Finding {
	var out []Finding
	if !providers.CredentialPresent(p) {
		out = append(out, Finding{Check: "credential", Profile: p.Name, Detail: "no stored credential for " + p.Name})
	}
	if reason, marked := st.AuthMarked(router.AccountOf(p), now); marked {
		out = append(out, Finding{Check: "auth", Profile: p.Name, Detail: p.Name + " is auth-marked: " + reason})
	}
	return out
}

// CheckModel is model as p serves it to agentName; failed names the check that refused it, note a check it skipped.
func CheckModel(agentName string, p *providers.Provider, model string, now time.Time) (wire string, failed *Finding, note string) {
	if model == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, model) {
		return model, nil, ""
	}
	fail := func(check, detail string) (string, *Finding, string) {
		return "", &Finding{Check: check, Profile: p.Name, Model: model, Detail: detail}, ""
	}
	why := ""
	if agentName == "claude" { // Serves covers external agents only
		if wire = providers.SpellPin(p, model); wire == "" {
			why = p.Name + " serves no " + model
		}
	} else {
		wire, why = ProfileServes(agentName, p, model)
	}
	switch {
	case why != "":
		return fail("served", why)
	case speaksResponses(agentName) && p.Preset == providers.PresetDatabricks && !providers.ResponsesServable(p, wire):
		return fail("wire", p.Name+" serves no "+model+" over Responses")
	case p.Preset != providers.PresetDatabricks:
		return wire, nil, ""
	}
	listed, ready, fresh := router.ServedListed(p, wire, now)
	switch {
	case !fresh:
		note = "served listing for " + p.Name + " is missing or older than 24h: endpoint check skipped (claude-code doctor --refresh)"
	case !listed:
		return fail("served", p.Name+" lists no endpoint "+wire)
	case !ready:
		return fail("served", wire+" is not a READY endpoint on "+p.Name)
	}
	return wire, nil, note
}

// explicitFinding is the first failed preflight check of an explicit Databricks backend, nil when it passes.
func explicitFinding(q RouteReq, p *providers.Provider) *Finding {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	now := time.Now()
	if fs := CheckWorkspace(p, loadState(), now); len(fs) > 0 {
		return &fs[0]
	}
	if q.Agent == "claude" {
		return nil
	}
	_, failed, note := CheckModel(q.Agent, p, strings.TrimPrefix(q.Model, p.Name+":"), now)
	if note != "" {
		refreshListing(p.Name)
	}
	return failed
}
