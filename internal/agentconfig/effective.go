package agentconfig

import (
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// geminiLogin is gemini's profile when unpinned: its own Google login, not a registry row.
const geminiLogin = "antigravity"

// Effective is an agent's effective settings after the launch flags are applied.
type Effective struct{ Agent, Profile, Model, Effort, Fallback string }

// Launch is the flags the caller typed plus the workspace's last-used profile; the zero value keeps every agent on its pin.
// Routed leads a router run instead (no flag means no profile, unknown profiles err); CanServe filters --provider picks.
type Launch struct {
	Profile, Provider, LastProfile string
	Routed                         bool
	CanServe                       func(agent string, p *providers.Provider) bool
}

// ErrUnknownProfile marks a profile that is no registry row; For still returns the Effective it resolved.
var ErrUnknownProfile = errors.New("not in the registry")

// lastProfileAgents are the agents whose CLI launch reuses the workspace's last-used profile before the pin.
var lastProfileAgents = []string{"codex", "copilot", "gemini", "opencode"}

// For is agent's pin with the launch flags, then the last-used profile, over Profile; a registry profile must exist
// (own logins skip the check). A vault read error is returned.
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
	cfg := Effective{Agent: agent, Profile: rec.Profile, Model: rec.Model, Effort: rec.Effort, Fallback: rec.Fallback}
	active := rec.Profile != "" // an off agent (no profile) never turns on from the last-used cache
	if cfg.Profile == "" && agent == "gemini" {
		cfg.Profile = geminiLogin
	}
	switch {
	case l.Profile != "" || l.Provider != "":
		lead, err := resolveLead(reg, agent, l)
		if err != nil {
			return Effective{}, fmt.Errorf("agent %s: %w", agent, err)
		}
		if lead != "" {
			cfg.Profile = lead // an empty lead keeps the agent's pin
		}
	case l.LastProfile != "" && active && slices.Contains(lastProfileAgents, agent) && reg.Providers[l.LastProfile] != nil:
		cfg.Profile = l.LastProfile
	}
	if p := cfg.Profile; p != "" && !ownsProfile(agent, p) && reg.Providers[p] == nil {
		return cfg, fmt.Errorf("agent %s: profile %s is %w", agent, p, ErrUnknownProfile)
	}
	return cfg, nil
}

// ownsProfile reports whether p is the agent's own login or an opencode sub id, neither a registry row.
func ownsProfile(agent, p string) bool {
	return p == vault.OwnLoginProfiles[agent] || agent == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, p)
}

// vaultSession is the --profile value naming the session's own profile (agent.LeadSession).
const vaultSession = "session"

// resolveLead is the profile a launch leads with: l.Profile ("session" = the session's), narrowed to l.Provider.
func resolveLead(reg *providers.Registry, agent string, l Launch) (string, error) {
	profile, provider := l.Profile, l.Provider
	fromSession := profile == vaultSession
	if fromSession {
		profile = providers.SessionLead(nil, true)
		// a session-inherited lead never moves gemini off its pin; only a named profile or provider does
		if p := reg.Providers[profile]; agent == "gemini" && provider == "" && (p == nil || p.Provider != "google") {
			return "", nil
		}
	}
	if profile != "" && reg.Providers[profile] == nil && l.Routed {
		return "", fmt.Errorf("profile %s is not in the registry", profile)
	}
	if provider == "" {
		return profile, nil
	}
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
