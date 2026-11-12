package agent

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
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
	return "preflight: " + e.Finding.String() + " (no fallback for this dispatch: --fallback or " + router.EnvFallback + ")"
}

// refreshListing starts one detached re-list of a workspace's listing, deduped per profile; a seam for tests.
var refreshListing = func(profile string) { catalog.SpawnDiscover(profile, false) }

// StubRefreshListingForTest turns the detached listing refresh off for a whole test binary (call from TestMain).
func StubRefreshListingForTest() { refreshListing = func(string) {} }

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
	if model == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, models.Strip1M(model)) {
		return model, nil, ""
	}
	fail := func(check, detail string) (string, *Finding, string) {
		return "", &Finding{Check: check, Profile: p.Name, Model: model, Detail: detail}, ""
	}
	served := router.SpellServed(p, model, now) // an unlisted id the workspace serves as databricks-<id>
	wire, why := preflightModelWire(agentName, p, served)
	spelled := cmp.Or(wire, providers.SpellPin(p, served)) // a Responses agent's wire check runs before Serves' wording
	switch {
	case preflightResponsesMismatch(agentName, p, spelled):
		return fail("wire", p.Name+" serves no "+model+" over Responses")
	case why != "":
		return fail("served", why)
	case p.Preset != providers.PresetDatabricks:
		return wire, nil, ""
	}
	note, why = preflightEndpoint(p, wire, now)
	if why != "" {
		return fail("served", why)
	}
	return wire, nil, note
}

// preflightResponsesMismatch preserves the Responses wire refusal before the served-model refusal.
func preflightResponsesMismatch(agentName string, p *providers.Provider, wire string) bool {
	return speaksResponses(agentName) && p.Preset == providers.PresetDatabricks &&
		wire != "" && !providers.ResponsesServable(p, wire)
}

// preflightModelWire permits agy's gateway transport without requiring a custom-base-URL dialect.
func preflightModelWire(agentName string, p *providers.Provider, model string) (wire, why string) {
	if agentName == "claude" || (agentName == "gemini" && GeminiGatewayRow(p)) {
		if wire = providers.SpellPin(p, model); wire == "" {
			why = p.Name + " serves no " + model
		}
	} else {
		wire, why = ProfileServes(agentName, p, model)
	}
	return wire, why
}

// preflightEndpoint checks the cached workspace listing without making a model call.
func preflightEndpoint(p *providers.Provider, wire string, now time.Time) (note, why string) {
	listed, ready, fresh := router.ServedListed(p, wire, now)
	switch {
	case !fresh:
		return "served listing for " + p.Name + " is missing or older than 24h: endpoint check skipped (claude-code doctor --refresh)", ""
	case !listed:
		return "", p.Name + " lists no endpoint " + wire
	case !ready:
		return "", wire + " is not a READY endpoint on " + p.Name
	}
	return "", ""
}

// CheckModelNoted is CheckModel whose skipped-check note starts one listing refresh and lands in pf (nil: stderr).
func CheckModelNoted(agentName string, p *providers.Provider, model string, now time.Time, pf *Preflight) (string, *Finding) {
	wire, failed, note := CheckModel(agentName, p, model, now)
	if note == "" {
		return wire, failed
	}
	refreshListing(p.Name)
	if pf != nil {
		pf.note(note)
	} else {
		fmt.Fprintln(os.Stderr, "preflight: note: "+note)
	}
	return wire, failed
}

// armFinding is the first failed local check of the overlay databricks:<p> workspace, recorded in rc.pf; nil passes.
func (rc routeCtx) armFinding() *Finding {
	p := rc.reg.Row(router.ArmProfile())
	if p == nil || p.Preset != providers.PresetDatabricks {
		return nil
	}
	fs := CheckWorkspace(p, loadState(), time.Now())
	if len(fs) == 0 {
		return nil
	}
	rc.pf.fail(fs[0])
	return &fs[0]
}

// databricksDispatch reports d billing a Databricks workspace of reg.
func databricksDispatch(reg *providers.Registry, d Dispatch) bool {
	_, profile, _ := strings.Cut(d.Account, "·")
	p := reg.Row(profile)
	return p != nil && p.Preset == providers.PresetDatabricks
}

// proxyNote is the note for a proxy that is not running; the launch starts it, so it is never a failure.
func proxyNote() string {
	if st, err := gateway.ReadState(); err == nil && st.PID > 0 && proc.Alive(st.PID) {
		return ""
	}
	return "proxy not running: the launch starts it (claude-code gateway proxy --status)"
}

// explicitFinding is the first failed preflight check of an explicit Databricks backend, nil when it passes.
func explicitFinding(q RouteReq, p *providers.Provider, pf *Preflight) *Finding {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	now := time.Now()
	if fs := CheckWorkspace(p, loadState(), now); len(fs) > 0 {
		return &fs[0]
	}
	_, failed := CheckModelNoted(q.Agent, p, strings.TrimPrefix(q.Model, p.Name+":"), now, pf)
	return failed
}

// modelFinding is explicitFinding for a --model <profile>:<model> with no --profile; nil when it names no row.
func modelFinding(q RouteReq, pf *Preflight) *Finding {
	name, _, ok := strings.Cut(q.Model, ":")
	if !ok {
		return nil
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[name] == nil {
		return nil
	}
	return explicitFinding(q, reg.Providers[name], pf)
}
