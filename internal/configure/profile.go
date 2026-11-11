// profile.go — ApplyProfile, the one action every profile change goes through: set the record, forget last-used.
package configure

import (
	"fmt"
	"io"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/preflight"
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
	return changed, providers.ClearLastProfilesFor(agents...)
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
	return providers.ClearLastProfiles()
}

// ProfileStatus is the one-line `<agent> profile` readout: the record's profile, identity and on/off state.
func ProfileStatus(agent string) string {
	line := agent + ": no profile set (launches use today's selection)"
	if v := vault.GetAgentRecord(agent).Profile; v != "" {
		line = fmt.Sprintf("%s: profile %s", agent, v) + providers.IdentityNote(v)
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

// KeyVerified live-probes a keyed provider; false when absent or not accepted.
func KeyVerified(p *providers.Provider) bool {
	if p == nil || !providers.CredentialPresent(p) {
		return false
	}
	status, _, _ := ProbeCached(KeyTarget{Provider: p})
	return status == "verified"
}

// AgentProfile is `<agent> profile`: clear the pin (an external agent's own login, not off), show it, or set it.
func AgentProfile(w io.Writer, name, profile string, clear bool) error {
	switch {
	case clear:
		if _, err := ApplyProfile([]string{name}, vault.OwnLoginProfiles[name]); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: profile cleared (own login)\n", name)
	case profile == "":
		fmt.Fprintln(w, ProfileStatus(name))
	default:
		return SetAgentProfile(w, name, profile)
	}
	return nil
}

// SetAgentProfile pins name to profile and prints what changed; copilot on a login-less github row gets a note.
func SetAgentProfile(w io.Writer, name, profile string) error {
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	if _, err := ApplyProfile([]string{name}, profile); err != nil {
		return fmt.Errorf("%w (see claude-code configure provider --list)", err)
	}
	if p := reg.Providers[profile]; name == "copilot" && p != nil && p.Provider == "github" && p.Login == "" {
		fmt.Fprintf(w, "copilot: %s has no login; billing follows gh's active account (set one with `vault edit`)\n", profile)
	}
	fmt.Fprintf(w, "%s: now on %s; relaunch `claude-code %s` to apply\n", name, profile, name)
	preflight.Warm(w, reg, time.Now())
	if name == "codex" {
		fmt.Fprintln(w, "plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return nil
}

// SetAgentActive is `<agent> on|off`: off clears the record's profile, on sets the own login; claude is always on.
func SetAgentActive(name string, on bool) error {
	value := agentpkg.AgentOff
	if on {
		value = agentpkg.AgentOn
	}
	if agentpkg.VaultAgentIsClaude(name) {
		return fmt.Errorf("%s is always on", name)
	}
	if err := agentpkg.ValidAgentActive(name, value); err != nil {
		return err
	}
	profile := ""
	if on {
		profile = vault.OwnLoginProfiles[name]
	}
	_, err := ApplyProfile([]string{name}, profile)
	return err
}
