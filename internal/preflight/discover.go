package preflight

import (
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// SpawnDiscover starts `preflight --discover <profile>` detached (a goroutine would die at a launch's exec); false when
// deduped by the profile's claim stamp. force skips the check but still stamps, so later unforced spawns dedup.
func SpawnDiscover(profile string, force bool) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	if !ClaimNamed("discover-"+url.PathEscape(profile), time.Now()) && !force {
		return false
	}
	return proc.SpawnDetached(self, []string{"preflight", "--discover", profile}, "", null)
}

// DiscoverTargets is the profiles copilot and opencode point at that have models to discover: Databricks or GitHub rows.
func DiscoverTargets(reg *providers.Registry) []string {
	var out []string
	for _, agent := range []string{agentid.Copilot, agentid.OpenCode} {
		name := providers.AgentRecord(agent).Profile
		if name == "" && agent == agentid.Copilot {
			name = GithubProfile(reg)
		}
		p := reg.Providers[name]
		if p == nil || (p.Preset != providers.PresetDatabricks && p.Provider != "github") || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// GithubProfile is the first profile on the github provider (copilot's default route), or "" when none exists.
func GithubProfile(reg *providers.Registry) string {
	if reg == nil {
		return ""
	}
	if names := reg.ProfilesOf("github"); len(names) > 0 {
		return names[0]
	}
	return ""
}

// DiscoverAfterAuth refreshes every discover target's model list in the background after a sign-in.
func DiscoverAfterAuth(reg *providers.Registry) {
	for _, t := range DiscoverTargets(reg) {
		SpawnDiscover(t, true)
	}
}

// RecallOrSpawn is p's sealed snapshot, no network, if usable accepts it; an expired one is served and spawn refreshes it.
func RecallOrSpawn(p *providers.Provider, usable func([]providers.ModelInfo) bool,
	spawn func(profile string) bool) ([]providers.ModelInfo, bool) {
	snap, ok := serving.Recall(p)
	if !ok || !usable(snap.Models) {
		return nil, false
	}
	if time.Since(snap.DiscoveredAt) >= serving.CacheTTL() {
		spawn(p.Name)
	}
	return snap.Models, true
}
