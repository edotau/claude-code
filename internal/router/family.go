package router

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

var familyIDRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:.*/)?(?:databricks-)?gpt-(\d+(?:[.-]\d+)*)-([a-z]+)$`)
})

// familyVersion is id's family and numeric version ("databricks-gpt-6-1-sol" → sol, [6 1]); ok is false for other ids.
func familyVersion(id string) (family string, version []int, ok bool) {
	m := familyIDRE().FindStringSubmatch(strings.ToLower(strings.TrimSpace(id)))
	if m == nil || !providers.IsFamily(m[2]) {
		return "", nil, false
	}
	for part := range strings.FieldsFuncSeq(m[1], func(r rune) bool { return r == '.' || r == '-' }) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return "", nil, false
		}
		version = append(version, n)
	}
	return m[2], version, true
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
		f, v, ok := familyVersion(id)
		if ok && f == strings.ToLower(family) && (best == "" || compareVersions(v, bestVer) > 0) {
			best, bestVer = id, v
		}
	}
	return best
}

// GithubCatalogIDs lists the ids of the GitHub Copilot catalog snapshot the harness keeps; the copilot package sets it.
var GithubCatalogIDs func() []string

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
		dead, st := deadAccounts(ProviderAccount(p.Name)), LoadState()
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
		return GithubCatalogIDs()
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

// inFamily reports whether id is a version of the family name m.
func inFamily(m, id string) bool {
	f, _, ok := familyVersion(id)
	return ok && providers.IsFamily(m) && f == strings.ToLower(m)
}
