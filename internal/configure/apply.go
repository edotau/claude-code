// apply.go — ApplyProfile, the one action every profile change goes through: set the record, forget last-used.
package configure

import (
	"fmt"

	"github.com/edotau/claude-code/internal/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// ApplyProfile sets profile as the Profile of each agent whose record differs, in one vault update that leaves every
// other field alone, then clears all targeted agents' last-used profiles. It returns the agents whose records changed.
// A non-empty profile must exist and be one each agent can serve; "" (off) always applies.
func ApplyProfile(agents []string, profile string) (changed []string, err error) {
	if err := validProfile(agents, profile); err != nil {
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

// validProfile rejects a profile some agent cannot run on, before any record is written.
func validProfile(agents []string, profile string) error {
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
	move, _ := SwitchTargets(reg, vault.AgentRecords(agentpkg.RosterNames()), profile)
	if _, err := ApplyProfile(move, profile); err != nil {
		return err
	}
	providers.AuditSwitch(profile)
	return agentpkg.ClearLastProfiles()
}
