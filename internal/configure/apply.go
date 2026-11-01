// apply.go — ApplyProfile, the one action every profile change goes through: set the record, forget last-used.
package configure

import (
	agentpkg "github.com/edotau/claude-code/internal/agent"
)

// ApplyProfile sets profile as the Profile of each agent whose record differs, in one vault update that leaves every
// other field alone, then clears the changed agents' last-used profile. It returns the agents it changed.
func ApplyProfile(agents []string, profile string) (changed []string, err error) {
	err = agentpkg.UpdateAgentRecords(func(m map[string]agentpkg.AgentRecord) {
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
	return changed, agentpkg.ClearLastProfilesFor(changed...)
}
