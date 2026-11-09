// Package agentcfg resolves one agent's effective configuration from its vault pin and the launch flags.
package agentcfg

import (
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// geminiLogin is gemini's profile when unpinned: its own Google login, not a registry row.
const geminiLogin = "antigravity"

// Config is an agent's effective settings after the launch flags are applied.
type Config struct{ Agent, Profile, Model, Effort, Fallback string }

// Launch is the flags the caller typed plus the workspace's last-used profile; the zero value keeps every agent on its pin.
type Launch struct{ Profile, Provider, LastProfile string }

// ErrUnknownProfile marks a profile that is no registry row; For still returns the Config it resolved.
var ErrUnknownProfile = errors.New("not in the registry")

// lastProfileAgents are the agents whose CLI launch reuses the workspace's last-used profile before the pin.
var lastProfileAgents = []string{"codex", "copilot", "gemini", "opencode"}

// For is agent's pin with the launch flags, then the last-used profile, over Profile; a registry profile must exist
// (own logins skip the check). A vault read error is returned.
func For(agent string, l Launch) (Config, error) {
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return Config{}, err
	}
	rec := recs[agent]
	cfg := Config{Agent: agent, Profile: rec.Profile, Model: rec.Model, Effort: rec.Effort, Fallback: rec.Fallback}
	active := rec.Profile != "" // an off agent (no profile) never turns on from the last-used cache
	if cfg.Profile == "" && agent == "gemini" {
		cfg.Profile = geminiLogin
	}
	if l.Provider != "" && l.Provider != "databricks" {
		return Config{}, fmt.Errorf("agent %s: --provider %s is not supported (only databricks)", agent, l.Provider)
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
	case l.Provider == "databricks":
		if cfg.Profile, err = databricksProfile(reg); err != nil {
			return Config{}, fmt.Errorf("agent %s: %w", agent, err)
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
