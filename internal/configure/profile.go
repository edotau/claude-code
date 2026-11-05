// apply.go — ApplyProfile, the one action every profile change goes through: set the record, forget last-used.
package configure

import (
	"fmt"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// ApplyProfile sets profile as the Profile of each agent whose record differs, in one vault update that leaves every
// other field alone, then clears all targeted agents' last-used profiles. It returns the agents whose records changed.
// A non-empty profile must exist and be one each agent can serve; "" (off) always applies.
func ApplyProfile(agents []string, profile string) (changed []string, err error) {
	if err := ValidProfile(agents, profile); err != nil {
		return nil, err
	}
	err = vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		changed = nil
		for _, a := range agents {
			if r := m[a]; r.Profile != profile {
				r.Profile = profile
				m[a] = r
				changed = append(changed, a)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return changed, agentpkg.ClearLastProfilesFor(agents...)
}

// ValidProfile rejects a profile some agent cannot run on, before any record is written.
func ValidProfile(agents []string, profile string) error {
	if profile == "" {
		return nil
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if err := agentpkg.ValidAgentPin(reg, a, profile); err != nil {
			return err
		}
		if row := reg.Providers[profile]; row != nil && a != "claude" && !agentpkg.CanServe(a, row) {
			return fmt.Errorf("%s cannot serve profile %q", a, profile)
		}
	}
	return nil
}

// SwitchTargets are the agents a switch to profile moves: claude, and each external agent that is on and serves it.
// Serves, not CanServe: a gemini gateway row is reachable only by an explicit `gemini profile`.
func SwitchTargets(reg *providers.Registry, recs map[string]vault.AgentRecord, profile string) (move, off []string) {
	for _, spec := range agentpkg.Roster() {
		ok, _ := agentpkg.Serves(spec.Name, reg.Providers[profile], "")
		switch {
		case !spec.External():
			move = append(move, spec.Name)
		case ok && recs[spec.Name].Profile == "":
			off = append(off, spec.Name)
		case ok:
			move = append(move, spec.Name)
		}
	}
	return move, off
}

// SwitchRecords sets profile on every SwitchTargets mover in m (the dry-run view of SwitchProfile).
func SwitchRecords(reg *providers.Registry, m map[string]vault.AgentRecord, profile string) {
	move, _ := SwitchTargets(reg, m, profile)
	for _, name := range move {
		r := m[name]
		r.Profile = profile
		m[name] = r
	}
}

// SwitchProfile moves claude and every on agent that serves profile onto it, audits it, and forgets last-used.
func SwitchProfile(reg *providers.Registry, profile string) error {
	recs, err := vault.ReadAgentRecords(agentpkg.RosterNames())
	if err != nil {
		return fmt.Errorf("switch profile: %w", err)
	}
	move, _ := SwitchTargets(reg, recs, profile)
	if _, err := ApplyProfile(move, profile); err != nil {
		return err
	}
	providers.AuditSwitch(profile)
	return agentpkg.ClearLastProfiles()
}

// ProfileStatus is the one-line `<agent> profile` readout: the record's profile, identity and on/off state.
func ProfileStatus(agent string) string {
	line := agent + ": no profile set (launches use today's selection)"
	if v := vault.GetAgentRecord(agent).Profile; v != "" {
		line = fmt.Sprintf("%s: profile %s", agent, v) + identityNote(v)
	}
	if !agentpkg.VaultAgentIsClaude(agent) {
		state := agentpkg.AgentOn
		if !agentpkg.AgentActive(agent) {
			state = agentpkg.AgentOff
		} else if why := agentpkg.AgentUnbillable(agent); why != "" {
			state = agentpkg.AgentOff + ", " + why
		}
		line += " · " + state
	}
	return line
}

// identityNote is " · identity <mode>" for a profile paired with a service principal, else "".
func identityNote(profile string) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	p, ok := reg.Providers[profile]
	if !ok || p == nil || p.Auth.SPClientID == "" {
		return ""
	}
	mode, err := providers.ResolveIdentity(p.Identity, "")
	if err != nil {
		return ""
	}
	return " · identity " + mode
}

// agentmenu.go — one submenu per agent CLI, all one shape: the two billing modes (Subscription, API key), then the
// actions that refresh or mint the credential (Login, then a token or key mint), then Back.

// KeyVerified live-probes a keyed provider; false when absent or not accepted.
func KeyVerified(p *providers.Provider) bool {
	if p == nil || !providers.CredentialPresent(p) {
		return false
	}
	status, _, _ := ProbeCached(KeyTarget{Provider: p})
	return status == "verified"
}

// KeyPresent reports whether provider name has a stored key.
func KeyPresent(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	p := ProfileBy(reg, provider, providers.AuthKey)
	return p, p != nil && providers.CredentialPresent(p)
}

// ProfileBy is the first profile under provider signing in by method (key, cli, ...), or nil.
func ProfileBy(reg *providers.Registry, provider, method string) *providers.Provider {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Method() == method {
			return p
		}
	}
	return nil
}

// ProfileName is p's name, "" for nil.
func ProfileName(p *providers.Provider) string {
	if p == nil {
		return ""
	}
	return p.Name
}
