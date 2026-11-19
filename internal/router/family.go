package router

import (
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

func init() {
	providers.FamilyPick = func(agent, family string, p *providers.Provider) string {
		return FamilyModel(agent, family, p, nil, time.Now())
	}
}

// compareVersions orders dotted versions numerically; a missing part counts as 0.
func compareVersions(a, b []int) int {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// FamilyLatest is the id of ids in family with the highest version, as listed; "" when none is a member.
func FamilyLatest(family string, ids []string) string {
	best, bestVer := "", []int(nil)
	for _, id := range ids {
		f, v, ok := providers.FamilyVersion(id)
		if ok && f == strings.ToLower(family) && (best == "" || compareVersions(v, bestVer) > 0) {
			best, bestVer = id, v
		}
	}
	return best
}

// GithubCatalogIDs lists the ids of the Copilot catalog snapshot kept for row p's account; the copilot package sets it.
var GithubCatalogIDs func(p *providers.Provider) []string

// GithubCatalogTime is the same cached snapshot's timestamp, zero on a miss.
var GithubCatalogTime func(p *providers.Provider) time.Time

// FamilyModel is the highest READY runnable version of family on p, else today's id; ids overrides a first-party list.
func FamilyModel(agent, family string, p *providers.Provider, ids []string, now time.Time) string {
	family = strings.ToLower(strings.TrimSpace(family))
	dbx := p != nil && p.Preset == providers.PresetDatabricks
	switch {
	case dbx:
		ids = servedIDs(p, now)
	case ids == nil:
		ids = firstPartyIDs(p)
	}
	if dbx {
		dead, st := deadAccounts(ProviderAccount, ProviderAccount(p.Name)), sharedState()
		ids = slices.DeleteFunc(slices.Clone(ids), func(id string) bool {
			return !bucketRunnable(agent, p, id) || tierUnusable(st, dead, p, id, now)
		})
	}
	if best := FamilyLatest(family, ids); best != "" {
		return best
	}
	if dbx {
		return SpellServed(p, providers.FamilyDefaults[family], now)
	}
	if p == nil {
		return providers.FamilyDefaults[family]
	}
	return p.WireModel(providers.FamilyDefaults[family])
}

// firstPartyIDs is the list the harness already keeps for p's sign-in row: copilot's snapshot, codex's cache.
func firstPartyIDs(p *providers.Provider) []string {
	switch {
	case p == nil:
		return nil
	case p.Provider == "github" && GithubCatalogIDs != nil:
		return GithubCatalogIDs(p)
	case p.Auth.Login == "codex":
		return providers.CodexCachedIDs()
	}
	return nil
}

// servedIDs is p's fresh listing's READY endpoint ids; nil without a fresh listing.
func servedIDs(p *providers.Provider, now time.Time) []string {
	s, ok := freshShared(p.Name, now)
	if !ok {
		return nil
	}
	var out []string
	for _, m := range s.Models {
		if m.IsReady() {
			out = append(out, m.ID)
		}
	}
	return out
}

// InFamily reports whether id is a version of the family name m.
func InFamily(m, id string) bool {
	f, _, ok := providers.FamilyVersion(id)
	return ok && providers.IsFamily(m) && f == strings.ToLower(m)
}
