// Package agentcfg resolves one agent's effective configuration from its vault pin and the launch flags.
package agentcfg

import (
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// geminiLogin is gemini's profile when unpinned: its own Google login, not a registry row.
const geminiLogin = "antigravity"

// Config is an agent's effective settings after the launch flags are applied.
type Config struct{ Agent, Profile, Model, Effort, Fallback string }

// Launch is the flags the caller typed; the zero value keeps every agent on its pin.
type Launch struct{ Profile, Provider string }

// For is agent's pin with the launch flags over Profile; a registry profile must exist (own logins skip the check).
func For(agent string, l Launch) (Config, error) {
	rec := providers.AgentRecord(agent)
	cfg := Config{Agent: agent, Profile: rec.Profile, Model: rec.Model, Effort: rec.Effort, Fallback: rec.Fallback}
	if cfg.Profile == "" && agent == "gemini" {
		cfg.Profile = geminiLogin
	}
	reg, err := providers.Load()
	if err != nil {
		return Config{}, err
	}
	switch {
	case l.Profile == vaultSession:
		if lead := providers.SessionLead(nil, true); lead != "" {
			cfg.Profile = lead // an empty lead keeps the agent's pin
		}
	case l.Profile != "":
		cfg.Profile = l.Profile
	case l.Provider != "" && l.Provider != "databricks":
		return Config{}, fmt.Errorf("agent %s: --provider %s is not supported (only databricks)", agent, l.Provider)
	case l.Provider == "databricks":
		if cfg.Profile, err = databricksProfile(reg); err != nil {
			return Config{}, fmt.Errorf("agent %s: %w", agent, err)
		}
	}
	if p := cfg.Profile; p != "" && !ownsProfile(agent, p) && reg.Providers[p] == nil {
		return Config{}, fmt.Errorf("agent %s: profile %s is not in the registry", agent, p)
	}
	return cfg, nil
}

// ownsProfile reports whether p is the agent's own login or an opencode sub id, neither a registry row.
func ownsProfile(agent, p string) bool {
	return p == vault.OwnLoginProfiles[agent] || agent == "opencode" && slices.Contains(providers.RouteOpencodeSubIDs, p)
}

// vaultSession is the --profile value naming the session's own profile (agent.LeadSession).
const vaultSession = "session"

// databricksProfile is the session profile if a signed-in databricks one, else the first signed-in databricks profile.
func databricksProfile(reg *providers.Registry) (string, error) {
	s := providers.SessionLead(nil, true)
	if p := reg.Providers[s]; p != nil && p.Provider == "databricks" && providers.CredentialPresent(p) {
		return s, nil
	}
	for _, n := range reg.ProfilesOf("databricks") {
		if providers.CredentialPresent(reg.Providers[n]) {
			return n, nil
		}
	}
	return "", fmt.Errorf("no signed-in databricks profile")
}
