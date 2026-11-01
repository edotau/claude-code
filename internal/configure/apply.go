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
