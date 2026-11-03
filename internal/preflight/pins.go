package preflight

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// agents are the third-party CLIs whose pins are checked.
var agents = []string{"codex", "copilot", "opencode"}

// Status values of a Pin.
const (
	StatusOK       = "ok"
	StatusUnpinned = "unpinned" // no profile resolves: the agent runs on its own login
	StatusStale    = "stale"    // the pin names a missing profile, or the cached listing is past its TTL
	StatusUnserved = "unserved" // the workspace does not serve the model (or the profile has no credential)
	StatusError    = "error"    // the vault could not be read; no pins were resolved
)

// Pin is one agent's resolved profile and model pin and what the workspace says about it.
type Pin struct {
	Agent         string `json:"agent"`
	Profile       string `json:"profile"`
	ProfileSource string `json:"profile_source"` // pin | follows CLAUDE_PROFILE | none
	Model         string `json:"model"`
	Served        string `json:"served"` // yes | no | unknown | n/a
	Status        string `json:"status"`
	Fix           string `json:"fix,omitempty"`
	Detail        string `json:"detail,omitempty"`
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

// Check resolves cached pins without network or writes; an unreadable vault returns one StatusError diagnostic.
func Check(reg *providers.Registry, now time.Time) []Pin {
	pins, err := agent.AgentPins()
	if err != nil {
		return []Pin{{Agent: "vault", Status: StatusError, Served: "no", Detail: err.Error()}}
	}
	session := pins["claude"].Profile
	var out []Pin
	if session != "" { // claude's session profile is always warmed
		out = append(out, checkAgent(reg, "claude", router.PinSpec{Profile: session}, session, now))
	}
	for _, a := range agents {
		spec := pins[a]
		if spec.Off {
			continue
		}
		out = append(out, checkAgent(reg, a, spec, session, now))
	}
	return out
}

func checkAgent(reg *providers.Registry, name string, spec router.PinSpec, session string, now time.Time) Pin {
	p := Pin{Agent: name, Profile: spec.Profile, ProfileSource: "pin", Model: spec.Model, Served: "n/a"}
	if p.Profile == "" && name == "codex" {
		p.Profile, p.ProfileSource = session, "follows CLAUDE_PROFILE"
	}
	if p.Profile == "" {
		p.ProfileSource, p.Status = "none", StatusUnpinned
		p.Detail = "own login, not Databricks"
		p.Fix = fmt.Sprintf("claude-code %s profile %s", name, cmp.Or(session, "<profile>"))
		return p
	}
	if name == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, p.Profile) {
		p.Status = StatusOK
		return p
	}
	prov := reg.Providers[p.Profile]
	if prov == nil {
		p.Status, p.Detail = StatusStale, "profile "+p.Profile+" is not in the registry"
		p.Fix = "claude-code configure provider --list; then claude-code " + name + " profile <profile>"
		return p
	}
	p.Status = StatusOK
	if !providers.CredentialPresent(prov) {
		p.Status, p.Served, p.Detail = StatusUnserved, "unknown", "no credential for "+p.Profile
		p.Fix = "claude-code configure provider --list " + p.Profile
		return p
	}
	if prov.Preset != providers.PresetDatabricks {
		return p
	}
	p.Served = "unknown"
	cat, ok := router.LoadServed(p.Profile)
	switch {
	case !ok:
		p.Detail = "workspace listing not cached"
		p.Fix = "claude-code doctor --refresh"
		if p.Model != "" {
			p.Status = StatusStale
		}
	case p.Model == "":
		p.Served = "n/a"
	default:
		listed, ready := cat.Has(prov, p.Model)
		p.Served = map[bool]string{true: "yes", false: "no"}[listed && ready]
		switch {
		case !cat.Fresh(now):
			p.Status, p.Detail = StatusStale, "workspace listing older than 24h"
			p.Fix = "claude-code doctor --refresh"
		case !listed || !ready:
			p.Status = StatusUnserved
			p.Detail = map[bool]string{true: "endpoint not READY", false: "not in the workspace listing"}[listed]
			p.Fix = fmt.Sprintf("claude-code models --live --profile %s; then claude-code configure (%s model)", p.Profile, name)
		}
	}
	return p
}

// profiles lists the Databricks profiles any checked pin resolves to, sorted and de-duplicated.
func profiles(reg *providers.Registry, ps []Pin) []*providers.Provider {
	var out []*providers.Provider
	seen := map[string]bool{}
	for _, p := range ps {
		prov := reg.Providers[p.Profile]
		if prov == nil || prov.Preset != providers.PresetDatabricks || seen[p.Profile] || !providers.CredentialPresent(prov) {
			continue
		}
		seen[p.Profile] = true
		out = append(out, prov)
	}
	slices.SortFunc(out, func(a, b *providers.Provider) int { return strings.Compare(a.Name, b.Name) })
	return out
}
