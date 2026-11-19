// rotation.go: the rotate pin's pool, served models on the agent's rows that meet the tier's ranks.json rule.
package agent

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

func init() { router.RotationPool = rotationPool }

// rotationTiers maps a route alias to the ranks.json rotation tier whose floor and cost cap it takes.
var rotationTiers = map[string]string{models.Haiku: "fast", models.Sonnet: "standard", models.Opus: "deep"}

// rotationPool is the served models agent runs on p that meet alias's rotation rule; "" alias is every tier's union.
func rotationPool(agentName, alias string, p *providers.Provider, now time.Time) []string {
	reg, err := providers.LoadShared()
	if err != nil || !router.RotationRow(p) {
		return nil
	}
	cands := rotationCandidates(reg, agentName, p, now)
	tiers := []string{"fast", "standard", "deep"}
	if t := rotationTiers[alias]; t != "" {
		tiers = []string{t}
	}
	var out []string
	for _, t := range tiers {
		for _, m := range catalog.RotationPool(cands, t, false) {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	if allow, ok := router.PoolOverlay(); ok {
		out = slices.DeleteFunc(out, func(m string) bool {
			return !slices.ContainsFunc(allow, func(a string) bool { return providers.SpellPin(p, a) == m })
		})
	}
	return out
}

// rotationCandidates is p's served chat models agent runs: a Databricks listing, else the Copilot catalog snapshot.
func rotationCandidates(reg *providers.Registry, agentName string, p *providers.Provider, now time.Time) []catalog.Candidate {
	if p.Preset == providers.PresetDatabricks {
		menu := providers.OpencodeMenu()
		cands, _ := autoCandidates(reg, ModelQuery{Agent: agentName, menu: &menu}, p, now)
		return cands
	}
	if router.GithubCatalogIDs == nil {
		return nil
	}
	var out []catalog.Candidate
	for _, id := range router.GithubCatalogIDs(p) {
		if catalog.ID(id) != AutoModel { // Copilot's own auto router is no model to rotate onto
			out = append(out, catalog.Candidate{Model: id})
		}
	}
	return out
}

// rotationRows are the rows agent rotates on: every serving workspace under the pool pin, else its pinned row.
func rotationRows(reg *providers.Registry, agentName string) []*providers.Provider {
	profile := strings.TrimSpace(vault.GetAgentRecord(agentName).Profile)
	if profile != PoolPin {
		if p := reg.Row(profile); router.RotationRow(p) {
			return []*providers.Provider{p}
		}
		return nil
	}
	var rows []*providers.Provider
	for _, n := range reg.ProfilesOf(providers.GroupDatabricks) {
		if p := reg.Providers[n]; CanServe(agentName, p) {
			rows = append(rows, p)
		}
	}
	return rows
}

// RotationModel is agent's next rotation model at alias across its rows; stamp records the pick, else it only peeks.
func RotationModel(reg *providers.Registry, agentName, alias string, stamp bool, now time.Time) (string, string, error) {
	rows := rotationRows(reg, agentName)
	if len(rows) == 0 {
		return "", "", nil
	}
	st := router.LoadState()
	var pool []string
	for _, p := range rows {
		for _, m := range rotationPool(agentName, alias, p, now) {
			if !slices.Contains(pool, m) && !st.ModelDead(router.AccountOf(p), m, now) {
				pool = append(pool, m)
			}
		}
	}
	account, p := router.AccountOf(rows[0]), rows[0]
	if len(rows) > 1 { // the pool pin: one history for every workspace, entries already wire ids
		account, p = router.ProviderAccount(PoolPin), nil
	}
	if !stamp {
		m := router.PeekRotation(agentName, account, p, pool, now)
		return m, rotationNote(m, alias, len(pool), "next"), nil
	}
	m, err := router.PickRotation(agentName, account, p, pool, now)
	return m, rotationNote(m, alias, len(pool), "picked"), err
}

// rotationNote names the pick, its tier and pool size, plus R5's review rule for a cheap or new-family author.
func rotationNote(m, alias string, n int, verb string) string {
	if m == "" {
		return ""
	}
	note := fmt.Sprintf("rotation %s %s (%s tier, %d in pool)", verb, m, rotationTiers[alias], n)
	rk, fam := catalog.RankOf(m), models.FamilyKind(m)
	if !rk.Known || rk.Capability < 5 || fam != models.FamilyClaude && fam != models.FamilyGPT {
		note += "; R5: full read and a capability-5 other-family review"
	}
	return note
}

// RotationFreeze records agent's next pick for brief's task tier: the --model a Run: line freezes so label == run.
func RotationFreeze(ctx context.Context, agentName, brief string) (string, string) {
	if agentModelPinContext(ctx, agentName) != router.DiscoveryModel {
		return "", ""
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return "", ""
	}
	alias, _ := TaskTier(brief)
	m, note, err := RotationModel(reg, agentName, cmp.Or(alias, models.Sonnet), true, time.Now())
	if err != nil {
		return "", "rotation state unavailable; the run picks at launch"
	}
	return m, note
}

// RotationSummary is a rotation pin's doctor detail: the pool size per tier, then the last 7 days' runs by model.
func RotationSummary(agentName string, now time.Time) string {
	reg, err := providers.LoadShared()
	if err != nil {
		return "rotating; registry unreadable"
	}
	rows := rotationRows(reg, agentName)
	var sizes []string
	for _, alias := range []string{models.Haiku, models.Sonnet, models.Opus} {
		n := 0
		for _, p := range rows {
			n = max(n, len(rotationPool(agentName, alias, p, now)))
		}
		sizes = append(sizes, fmt.Sprintf("%s %d", rotationTiers[alias], n))
	}
	return "rotating " + strings.Join(sizes, ", ") + "; 7d " + recentPicks(agentName, now)
}

// recentPicks is agent's last 7 days of runs counted by model, top four: "kimi-k3 3, gpt-6-2-sol 2".
func recentPicks(agentName string, now time.Time) string {
	counts := map[string]int{}
	for _, r := range RecentRecords(AllRecords) {
		m := cmp.Or(r.Invoked, r.Model)
		if r.Agent != agentName || m == "" || now.Sub(r.Time) >= 7*24*time.Hour {
			continue
		}
		if _, after, ok := strings.Cut(m, ":"); ok {
			m = after
		}
		counts[catalog.ID(m)]++
	}
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	var out []string
	for _, k := range keys[:min(len(keys), 4)] {
		out = append(out, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return cmp.Or(strings.Join(out, ", "), "no runs")
}
