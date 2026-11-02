package preflight

import (
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// SpawnDiscover starts `preflight --discover <profile>` detached (a goroutine would die at a launch's exec); false when
// deduped by the profile's claim stamp. force skips the check but still stamps, so later unforced spawns dedup.
func SpawnDiscover(profile string, force bool) bool {
	claimed := ClaimNamed("discover-"+url.PathEscape(profile), time.Now())
	if !force && !claimed {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	return proc.SpawnDetached(self, []string{"preflight", "--discover", profile}, "", null)
}

// DiscoverTargets is the profiles copilot and opencode point at that have models to discover: Databricks or GitHub rows.
func DiscoverTargets(reg *providers.Registry) []string {
	var out []string
	for _, agent := range []string{"copilot", "opencode"} {
		name := providers.AgentRecord(agent).Profile
		if name == "" && agent == "copilot" {
			name = "github"
		}
		p := reg.Providers[name]
		if p == nil || (p.Preset != providers.PresetDatabricks && p.Provider != "github") || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// DiscoverAfterAuth refreshes every discover target's model list in the background after a sign-in.
func DiscoverAfterAuth(reg *providers.Registry) {
	for _, t := range DiscoverTargets(reg) {
		SpawnDiscover(t, true)
	}
}
