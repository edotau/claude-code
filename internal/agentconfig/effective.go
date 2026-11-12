package agentconfig

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// geminiLogin is gemini's profile when unpinned: its own Google login, not a registry row.
const geminiLogin = "antigravity"

// Effective is an agent's effective settings after the launch flags are applied.
type Effective struct {
	Agent string
	vault.AgentRecord
}

// Launch is the typed flags plus the last-used profile; Routed leads a router run, CanServe filters --provider picks.
type Launch struct {
	Profile, Provider, LastProfile string
	Routed                         bool
	CanServe                       func(agent string, p *providers.Provider) bool
}

// ErrUnknownProfile marks a profile that is no registry row; For still returns the Effective it resolved.
var ErrUnknownProfile = errors.New("not in the registry")

// For is agent's pin overlaid by the launch flags, then the last-used profile; a registry profile must exist.
func For(agent string, l Launch) (Effective, error) {
	reg, err := providers.Load()
	if err != nil {
		return Effective{}, err
	}
	if l.Routed {
		lead, err := resolveLead(reg, agent, l)
		return Effective{Agent: agent, Profile: lead}, err
	}
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return Effective{}, err
	}
	rec := recs[agent]
	cfg := Effective{Agent: agent, AgentRecord: rec}
	if cfg.Profile == "" && agent == agentid.Gemini {
		cfg.Profile = geminiLogin
	}
	if cfg.Profile, err = launchProfile(reg, agent, l, cfg.Profile, rec.Profile != ""); err != nil {
		return Effective{}, err
	}
	return cfg, checkProfile(reg, agent, cfg.Profile)
}

// launchProfile is cur after the launch flags, else the last-used profile; active is false for an off agent (no pin).
func launchProfile(reg *providers.Registry, agent string, l Launch, cur string, active bool) (string, error) {
	switch {
	case l.Profile != "" || l.Provider != "":
		lead, err := resolveLead(reg, agent, l)
		if err != nil {
			return "", fmt.Errorf("agent %s: %w", agent, err)
		}
		return cmp.Or(lead, cur), nil // an empty lead keeps the agent's pin
	case l.LastProfile != "" && active && slices.Contains(agentid.External(), agent) && reg.Providers[l.LastProfile] != nil:
		return l.LastProfile, nil
	}
	return cur, nil
}

// checkProfile refuses a registry profile that does not exist; own logins skip the check.
func checkProfile(reg *providers.Registry, agent, p string) error {
	if p != "" && !ownsProfile(agent, p) && reg.Providers[p] == nil {
		return fmt.Errorf("agent %s: profile %s is %w", agent, p, ErrUnknownProfile)
	}
	return nil
}

// ownsProfile reports whether p is the agent's own login or an opencode sub id, neither a registry row.
func ownsProfile(agent, p string) bool {
	return p == vault.OwnLoginProfiles[agent] || agent == agentid.OpenCode && slices.Contains(providers.RouteOpencodeSubIDs, p)
}

// vaultSession is the --profile value naming the session's own profile (agent.LeadSession).
const vaultSession = "session"

// resolveLead is the profile a launch leads with: l.Profile ("session" = the session's), narrowed to l.Provider.
func resolveLead(reg *providers.Registry, agent string, l Launch) (string, error) {
	profile := l.Profile
	fromSession := profile == vaultSession
	if fromSession {
		profile = providers.SessionLead(nil, true)
		if keepsGeminiPin(reg, agent, profile, l.Provider) {
			return "", nil
		}
	}
	if profile != "" && reg.Providers[profile] == nil && l.Routed {
		return "", fmt.Errorf("profile %s is not in the registry", profile)
	}
	if l.Provider == "" {
		return profile, nil
	}
	return leadForProvider(reg, agent, l, profile, fromSession)
}

// keepsGeminiPin reports a session-inherited lead, which never moves gemini off its pin.
func keepsGeminiPin(reg *providers.Registry, agent, profile, provider string) bool {
	p := reg.Providers[profile]
	return agent == agentid.Gemini && provider == "" && (p == nil || p.Provider != "google")
}

// leadForProvider narrows profile to l.Provider: kept if it is one, else the first signed-in profile serving agent.
func leadForProvider(reg *providers.Registry, agent string, l Launch, profile string, fromSession bool) (string, error) {
	provider := l.Provider
	if p := reg.Providers[profile]; p != nil && p.Provider == provider {
		return profile, nil
	}
	if profile != "" && !fromSession {
		if reg.Providers[profile] == nil && !ownsProfile(agent, profile) {
			return "", fmt.Errorf("--profile %s is %w", profile, ErrUnknownProfile)
		}
		return "", fmt.Errorf("--profile %s is not a %s profile", profile, provider)
	}
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; serves(l, agent, p) && providers.CredentialPresent(p) {
			return n, nil
		}
	}
	return "", fmt.Errorf("no signed-in %s profile serves %s", provider, agent)
}

// serves is l.CanServe; with none set (a non-routed launch) only databricks rows qualify, as every agent can use them.
func serves(l Launch, agent string, p *providers.Provider) bool {
	if l.CanServe == nil {
		return p.Provider == "databricks"
	}
	return l.CanServe(agent, p)
}
