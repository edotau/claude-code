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
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Status values of a Pin.
const (
	StatusOK       = "ok"
	statusUnpinned = "unpinned" // no profile resolves at all
	statusStale    = "stale"    // the pin names a missing profile, or the cached listing is past its TTL
	statusUnserved = "unserved" // the workspace does not serve the model (or the profile has no credential)
	StatusError    = "error"    // the vault could not be read; no pins were resolved
)

// Pin is one agent's resolved profile and model pin and what the workspace says about it.
type Pin struct {
	Agent         string
	Profile       string
	ProfileSource string // pin | own login | pool | none
	Model         string
	Served        string // yes | no | unknown | n/a
	Status        string
	Fix           string
	Detail        string
	Also          []string // other profiles this row's alias pins use, warmed with Profile

	rotationDetail func() string
}

// Line is the one-line row: agent, profile, model, served, status and the fix.
func (p Pin) Line() string {
	s := fmt.Sprintf("profile=%s (%s) model=%s served=%s", cmp.Or(p.Profile, "-"), p.ProfileSource, cmp.Or(p.Model, "(none)"), p.Served)
	if p.Detail != "" {
		s += " · " + p.Detail
	} else if p.rotationDetail != nil {
		s += " · " + p.rotationDetail()
	}
	if p.Fix != "" {
		s += " · fix: " + p.Fix
	}
	return s
}

// Check resolves cached pins from ctx's run snapshot with no network or writes; an unreadable vault is one StatusError.
func Check(ctx context.Context, reg *providers.Registry, now time.Time) []Pin {
	pins, err := agent.AgentPinsContext(ctx)
	if err != nil {
		return []Pin{{Agent: "vault", Status: StatusError, Served: "no", Detail: err.Error()}}
	}
	recs, err := vault.ReadAgentRecords(router.TableAgents)
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
		pinned := recs[a].EffectiveRoute()
		switch spec := pins[a]; {
		case a == "claude" || spec.Off:
		case spec.Profile == "" && pinned.Profile != "": // AgentPins clears an own-login record profile
			out = append(out, loginRow(a, route.Target{Route: pinned, Model: spec.Model}, now))
		default:
			out = append(out, checkAgent(reg, a, spec, session, now))
		}
	}
	return out
}

// loginRow is the row of an agent on its own login or the pool pin; a rotation pin carries the rotation summary.
func loginRow(a string, target route.Target, now time.Time) Pin {
	profile, src := target.Profile, "own login"
	if target.Provider == providers.GroupDatabricks && profile == "auto" {
		profile, src = agent.PoolPin, "pool"
	}
	row := Pin{Agent: a, Profile: profile, ProfileSource: src, Model: target.Model, Served: "n/a", Status: StatusOK}
	if target.Model == agent.AutoModel {
		row.rotationDetail = func() string { return agent.RotationSummary(a, now) }
	}
	return row
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
	var ignored []string
	for _, alias := range models.AliasNames {
		spec, legacy := aliasPin(reg, rec, alias)
		ignored = appendNew(ignored, legacy...)
		if spec == "" {
			continue
		}
		t := providers.PlainTarget(def, spec)
		row.Also = append(row.Also, t.Provider.Name)
		pin := checkAliasPin(reg, alias, t, session, now)
		if pin.Status != StatusOK && !failed {
			failed = true
			row.Model, row.Served, row.Status, row.Fix = pin.Model, pin.Served, pin.Status, pin.Fix
			row.Detail = alias + " pin: " + cmp.Or(pin.Detail, pin.Status)
		}
	}
	if len(ignored) > 0 && row.Detail == "" {
		row.Detail = "ignored legacy cross-profile pin(s): " + strings.Join(ignored, ", ")
		row.Fix = clearPinsFix(ignored)
	}
	return row, nil
}

// clearPinsFix clears only the ignored `<slot>=<pin>` entries, keeping every other pin (--unpin clears them all).
func clearPinsFix(ignored []string) string {
	var fix strings.Builder
	fix.WriteString("claude-code models")
	for _, e := range ignored {
		slot, _, _ := strings.Cut(e, "=")
		fix.WriteString(" --pin " + slot + "=")
	}
	return fix.String()
}

// appendNew appends each of vs not already in dst.
func appendNew(dst []string, vs ...string) []string {
	for _, v := range vs {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

// checkAliasPin checks one alias pin's target on its profile and against Claude Code's own model rules.
func checkAliasPin(reg *providers.Registry, alias string, t providers.Target, session string, now time.Time) Pin {
	pin := checkAgent(reg, "claude", router.PinSpec{Profile: t.Provider.Name, Model: t.Model}, session, now)
	if cerr := providers.ClaudeCodeError(t.Provider, t.Model); cerr != nil {
		pin.Status, pin.Served, pin.Detail, pin.Fix = statusUnserved, "no", cerr.Error(), "claude-code models --pin "+alias+"=<id>"
	}
	return pin
}

// aliasPin is alias's pin as Select reads it (the alias pin, then the all-slot pin) and the skipped legacy `<slot>=<pin>`s.
func aliasPin(reg *providers.Registry, rec vault.AgentRecord, alias string) (spec string, legacy []string) {
	for i, pin := range []string{rec.Aliases[alias], rec.Model} {
		plain, isLegacy := sessionPin(reg, pin)
		if isLegacy {
			legacy = append(legacy, []string{alias, "all"}[i]+"="+strings.TrimSpace(pin))
		}
		if plain != "" {
			return plain, legacy
		}
	}
	return "", legacy
}

// sessionPin reads a stored pin as Select does: a plain id on the session profile, a profile-prefixed one is legacy.
func sessionPin(reg *providers.Registry, pin string) (plain string, legacy bool) {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return "", false
	}
	if providers.CheckModel(reg, pin) != nil {
		return "", true
	}
	return pin, false
}

func checkAgent(reg *providers.Registry, name string, spec router.PinSpec, session string, now time.Time) Pin {
	p := Pin{Agent: name, Profile: spec.Profile, ProfileSource: "pin", Model: spec.Model, Served: "n/a"}
	if p.Profile == "" {
		p.ProfileSource, p.Status = "none", statusUnpinned
		p.Detail = "no profile pinned"
		p.Fix = fmt.Sprintf("claude-code %s profile %s", name, cmp.Or(session, "<profile>"))
		return p
	}
	if agentid.Routing(name).SubLadder && slices.Contains(providers.RouteOpencodeSubIDs, p.Profile) {
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
	if prov.Preset == providers.PresetDatabricks && p.Model != agent.AutoModel {
		checkServed(&p, prov, name, now)
	}
	if p.Model == agent.AutoModel {
		p.Served = "n/a"
		p.rotationDetail = func() string { return agent.RotationSummary(name, now) }
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
	if base, _, ok := agentid.SplitNativeEffort(model); agentid.Routing(agent).EffortSuffix && ok {
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
