package agentconfig

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// geminiLogin is gemini's profile when unpinned: its own Google login, not a registry row.
const geminiLogin = "antigravity"

// Effective is an agent's effective settings after the launch flags are applied.
type Effective struct {
	Agent string
	vault.AgentRecord
}

// Launch is the typed flags; Routed leads a router run, CanServe filters --provider picks.
type Launch struct {
	Profile, Provider string
	Routed            bool
	CanServe          func(agent string, p *providers.Provider) bool
}

// ErrUnknownProfile marks a profile that is no registry row; ForContext still returns the Effective it resolved.
var ErrUnknownProfile = errors.New("not in the registry")

// checkProfile refuses a registry profile that does not exist; own logins skip the check.
func checkProfile(reg *providers.Registry, agent, p string) error {
	if p != "" && !ownsProfile(agent, p) && reg.Providers[p] == nil {
		return fmt.Errorf("agent %s: profile %s is %w", agent, p, ErrUnknownProfile)
	}
	return nil
}

// unknownProfile is the refusal for a typed profile no row has; auto names the overlay form.
func unknownProfile(profile string) error {
	if profile == route.AutoArm {
		return fmt.Errorf(`profile auto is %w; auto is an overlay value: --overlay '{"provider":"databricks","profile":"auto"}'`,
			ErrUnknownProfile)
	}
	return fmt.Errorf("profile %s is %w", profile, ErrUnknownProfile)
}

// CheckTypedProfile refuses a typed --profile that is no row; auto is valid only in an overlay.
func CheckTypedProfile(reg *providers.Registry, agent, profile string) error {
	if profile == "" || profile == vaultSession || strings.HasPrefix(profile, router.RouteDatabricks+":") || ownsProfile(agent, profile) {
		return nil
	}
	if _, err := reg.Get(profile); err != nil {
		return unknownProfile(profile)
	}
	return nil
}

// ownsProfile reports whether p is the agent's own login or an opencode sub id, neither a registry row.
func ownsProfile(agent, p string) bool {
	return p == vault.OwnLoginProfiles[agent] || agentid.Routing(agent).SubLadder && slices.Contains(providers.RouteOpencodeSubIDs, p)
}

// vaultSession is the --profile value naming the session's own profile (agent.LeadSession).
const vaultSession = "session"

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
	return signedProviderLead(reg, agent, l)
}

func signedProviderLead(reg *providers.Registry, agent string, l Launch) (string, error) {
	provider := l.Provider
	first := ""
	eligible := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; serves(l, agent, p) && providers.CredentialPresent(p) {
			if provider != router.RouteDatabricks {
				return n, nil
			}
			first = cmp.Or(first, n)
			if p.Preset == providers.PresetDatabricks {
				eligible.Providers[n] = p
			}
		}
	}
	if first == "" {
		return "", fmt.Errorf("no signed-in %s profile serves %s", provider, agent)
	}
	// The ranker only takes preset rows; with none, or on its error, the first signed-in profile leads as before.
	if names := rankLead(eligible); len(names) > 0 {
		return names[0], nil
	}
	return first, nil
}

func rankLead(eligible *providers.Registry) []string {
	if len(eligible.Providers) == 0 {
		return nil
	}
	st := router.LoadState()
	q := router.WorkspaceQuery[struct{}]{
		Findings: func(*providers.Provider, string, string, time.Time) []struct{} { return nil },
		Cooling: func(p *providers.Provider, now time.Time) bool {
			return st.CoolingUntil(router.AccountOf(p)).After(now)
		},
	}
	names, _, err := router.RankWorkspaces(eligible, q, time.Now())
	if err != nil {
		return nil
	}
	return names
}

// serves is l.CanServe; with none set (a non-routed launch) only databricks rows qualify, as every agent can use them.
func serves(l Launch, agent string, p *providers.Provider) bool {
	if l.CanServe == nil {
		return p.Provider == "databricks"
	}
	return l.CanServe(agent, p)
}

// ForContext is agent's pin overlaid by the launch flags; a registry profile must exist.
func ForContext(ctx context.Context, agent string, l Launch) (Effective, error) {
	reg, err := providers.Load()
	if err != nil {
		return Effective{}, err
	}
	if l.Routed {
		lead, err := resolveLead(ctx, reg, agent, l)
		return Effective{Agent: agent, Profile: lead}, err
	}
	recs, err := vault.ReadAgentRecords([]string{agent})
	if err != nil {
		return Effective{}, err
	}
	rec := recs[agent]
	cfg := Effective{Agent: agent, AgentRecord: rec}
	cfg.Profile = pinProfile(rec.EffectiveRoute())
	if cfg.Profile == "" && agent == agentid.Gemini {
		cfg.Profile = geminiLogin
	}
	if cfg.Profile, err = launchProfile(ctx, reg, cfg, l); err != nil {
		return Effective{}, err
	}
	return cfg, checkProfile(reg, agent, cfg.Profile)
}

// pinProfile is a pinned route as the profile a launch leads with; the pool keeps its group name.
func pinProfile(rt route.Route) string {
	if rt.Provider == providers.GroupDatabricks && rt.Profile == route.AutoArm {
		return providers.GroupDatabricks
	}
	return rt.Profile
}

func launchProfile(ctx context.Context, reg *providers.Registry, cfg Effective, l Launch) (string, error) {
	switch {
	case l.Profile != "" || l.Provider != "":
		lead, err := resolveLead(ctx, reg, cfg.Agent, l)
		if err != nil {
			return "", fmt.Errorf("agent %s: %w", cfg.Agent, err)
		}
		return cmp.Or(lead, cfg.Profile), nil
	}
	return cfg.Profile, nil
}

// resolveLead is the profile a launch leads with: l.Profile ("session" = the run's), narrowed to l.Provider.
func resolveLead(ctx context.Context, reg *providers.Registry, agent string, l Launch) (string, error) {
	profile := l.Profile
	fromSession := profile == vaultSession
	if fromSession {
		profile = providers.SessionLeadContext(ctx, nil, true)
		if keepsGeminiPin(reg, agent, profile, l.Provider) {
			return "", nil
		}
	}
	if profile != "" && reg.Providers[profile] == nil && l.Routed {
		return "", unknownProfile(profile)
	}
	if l.Provider == "" {
		return profile, nil
	}
	return leadForProvider(reg, agent, l, profile, fromSession)
}
