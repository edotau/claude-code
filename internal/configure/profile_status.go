package configure

import (
	"fmt"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

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
