package preflight

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Status values of a Pin.
const (
	StatusOK       = "ok"
	statusUnpinned = "unpinned" // no profile resolves: the agent runs on its own login
	statusStale    = "stale"    // the pin names a missing profile, or the cached listing is past its TTL
	statusUnserved = "unserved" // the workspace does not serve the model (or the profile has no credential)
	StatusError    = "error"    // the vault could not be read; no pins were resolved
)

// Pin is one agent's resolved profile and model pin and what the workspace says about it.
type Pin struct {
	Agent         string
	Profile       string
	ProfileSource string // pin | follows CLAUDE_PROFILE | none
	Model         string
	Served        string // yes | no | unknown | n/a
	Status        string
	Fix           string
	Detail        string
	Also          []string // other profiles this row's alias pins use, warmed with Profile
}

// Line is the one-line row: agent, profile, model, served, status and the fix.
func (p Pin) Line() string {
	s := fmt.Sprintf("profile=%s (%s) model=%s served=%s", cmp.Or(p.Profile, "-"), p.ProfileSource, cmp.Or(p.Model, "(none)"), p.Served)
	if p.Detail != "" {
		s += " · " + p.Detail
	}
	if p.Fix != "" {
		s += " · fix: " + p.Fix
	}
	return s
}

// Check resolves cached pins from ctx's run snapshot (the process environment when it has none) without network or
// writes; an unreadable vault returns one StatusError diagnostic.
func Check(ctx context.Context, reg *providers.Registry, now time.Time) []Pin {
	pins, err := agent.AgentPinsContext(ctx)
	if err != nil {
		return []Pin{{Agent: "vault", Status: StatusError, Served: "no", Detail: err.Error()}}
	}
	session := pins["claude"].Profile
	var out []Pin
	if session != "" { // claude's session profile is always warmed
		row, err := checkClaude(reg, session, now)
		if err != nil {
			return []Pin{{Agent: "vault", Status: StatusError, Served: "no", Detail: err.Error()}}
		}
		out = append(out, row)
	}
	for _, a := range slices.Sorted(slices.Values(router.TableAgents)) {
		if a != "claude" && !pins[a].Off {
			out = append(out, checkAgent(reg, a, pins[a], session, now))
		}
	}
	return out
}

// checkClaude reports the session row, carrying the first failing alias pin's status; every alias profile is warmed.
func checkClaude(reg *providers.Registry, session string, now time.Time) (Pin, error) {
	row := checkAgent(reg, "claude", router.PinSpec{Profile: session}, session, now)
	recs, err := vault.ReadAgentRecords([]string{"claude"})
	def := reg.Providers[session]
	if err != nil || def == nil || row.Status != StatusOK {
		return row, err
	}
	rec := recs["claude"]
	failed := false
	for _, alias := range models.AliasNames {
		spec := cmp.Or(strings.TrimSpace(rec.Aliases[alias]), strings.TrimSpace(rec.Model))
		if spec == "" {
			continue
		}
		t := providers.ParseTarget(reg, spec, def)
		row.Also = append(row.Also, t.Provider.Name)
		pin := checkAgent(reg, "claude", router.PinSpec{Profile: t.Provider.Name, Model: t.Model}, session, now)
		if cerr := providers.ClaudeCodeError(t.Provider, t.Model); cerr != nil {
			pin.Status, pin.Served, pin.Detail, pin.Fix = statusUnserved, "no", cerr.Error(), "claude-code models --pin "+alias+"=<id>"
		}
		if pin.Status != StatusOK && !failed {
			failed = true
			row.Model, row.Served, row.Status, row.Fix = pin.Model, pin.Served, pin.Status, pin.Fix
			row.Detail = alias + " pin: " + cmp.Or(pin.Detail, pin.Status)
		}
	}
	return row, nil
}

func checkAgent(reg *providers.Registry, name string, spec router.PinSpec, session string, now time.Time) Pin {
	p := Pin{Agent: name, Profile: spec.Profile, ProfileSource: "pin", Model: spec.Model, Served: "n/a"}
	if p.Profile == "" && name == "codex" {
		p.Profile, p.ProfileSource = session, "follows CLAUDE_PROFILE"
	}
	if p.Profile == "" && name == "gemini" { // gemini's pin is its own Google login, cleared by AgentPins
		p.Profile = "antigravity"
	}
	if p.Profile == "" {
		p.ProfileSource, p.Status = "none", statusUnpinned
		p.Detail = "own login, not Databricks"
		p.Fix = fmt.Sprintf("claude-code %s profile %s", name, cmp.Or(session, "<profile>"))
		return p
	}
	if name == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, p.Profile) {
		p.Status = StatusOK
		return p
	}
	return checkProfile(reg, p, name, now)
}

func checkProfile(reg *providers.Registry, p Pin, name string, now time.Time) Pin {
	prov := reg.Providers[p.Profile]
	if prov == nil {
		p.Status, p.Detail = statusStale, "profile "+p.Profile+" is not in the registry"
		p.Fix = "claude-code configure provider --list; then claude-code " + name + " profile <profile>"
		return p
	}
	p.Status = StatusOK
	if !providers.CredentialPresent(prov) {
		p.Status, p.Served, p.Detail = statusUnserved, "unknown", "no credential for "+p.Profile
		p.Fix = "claude-code configure provider --list " + p.Profile
		return p
	}
	if prov.Preset == providers.PresetDatabricks {
		checkServed(&p, prov, name, now)
	}
	return p
}

// checkServed fills p's served state for a Databricks pin from the cached workspace listing.
func checkServed(p *Pin, prov *providers.Provider, name string, now time.Time) {
	p.Served = "unknown"
	cat, ok := router.LoadServed(p.Profile)
	switch {
	case !ok:
		p.Detail = "workspace listing not cached"
		p.Fix = "claude-code doctor --refresh"
		if p.Model != "" {
			p.Status = statusStale
		}
	case p.Model == "":
		p.Served = "n/a"
	default:
		listed, ready := cat.Has(prov, servedName(name, p.Model))
		p.Served = map[bool]string{true: "yes", false: "no"}[listed && ready]
		switch {
		case !cat.Fresh(now):
			p.Status, p.Detail = statusStale, "workspace listing older than 24h"
			p.Fix = "claude-code doctor --refresh"
		case !listed || !ready:
			p.Status = statusUnserved
			p.Detail = map[bool]string{true: "endpoint not READY", false: "not in the workspace listing"}[listed]
			p.Fix = fmt.Sprintf("claude-code models --live --profile %s; then claude-code configure (%s model)", p.Profile, name)
		}
	}
}

// servedName is the model's serving-endpoint name: gemini's effort suffix (gemini-3.8-flash-high) is not part of it.
func servedName(agent, model string) string {
	if base, effort, ok := strings.CutLast(model, "-"); agent == agentid.Gemini && ok &&
		slices.Contains(agentid.NativeEfforts(agentid.Gemini), effort) {
		return base
	}
	return model
}

// profiles lists the Databricks profiles any checked pin resolves to, sorted and de-duplicated.
func profiles(reg *providers.Registry, ps []Pin) []*providers.Provider {
	var out []*providers.Provider
	seen := map[string]bool{}
	for _, p := range ps {
		for _, name := range append([]string{p.Profile}, p.Also...) {
			prov := reg.Providers[name]
			if prov == nil || prov.Preset != providers.PresetDatabricks || seen[name] || !providers.CredentialPresent(prov) {
				continue
			}
			seen[name] = true
			out = append(out, prov)
		}
	}
	slices.SortFunc(out, func(a, b *providers.Provider) int { return strings.Compare(a.Name, b.Name) })
	return out
}
