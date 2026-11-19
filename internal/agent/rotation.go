// rotation.go: auto's rotation pool, served models on the agent's rows that meet the tier's ranks.json rule.
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

type peekKey struct{}

// PeekContext marks ctx's route resolution as a label: a rotation pick is peeked, never recorded.
func PeekContext(ctx context.Context) context.Context { return context.WithValue(ctx, peekKey{}, true) }

func peekingRotation(ctx context.Context) bool { v, _ := ctx.Value(peekKey{}).(bool); return v }

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
	return rotationModelOn(rotationRows(reg, agentName), agentName, alias, stamp, now)
}

// rotationModelOn is RotationModel over the given rows.
func rotationModelOn(rows []*providers.Provider, agentName, alias string, stamp bool, now time.Time) (string, string, error) {
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
	n := takeable(router.LoadState()[account], agentName, p, pool, now)
	if !stamp {
		m := router.PeekRotation(agentName, account, p, pool, now)
		return m, rotationNote(m, alias, n, "next"), nil
	}
	m, err := router.PickRotation(agentName, account, p, pool, now)
	return m, rotationNote(m, alias, n, "picked"), err
}

// takeable counts pool entries a pick could take: served and not on a capability skip.
func takeable(e router.Entry, agentName string, p *providers.Provider, pool []string, now time.Time) int {
	if p != nil {
		pool = router.ServedPool(p, pool, now)
	}
	return len(pool) - len(slices.DeleteFunc(slices.Clone(pool), func(m string) bool {
		at, ok := e.DiscoverySkip[router.DiscoverySkipKey(agentName, m)]
		return !ok || now.Sub(at) >= router.DiscoverySkipTTL
	}))
}

// PoolMember is one rotation-pool model: the next pick, a live hold, and when it was last picked.
type PoolMember struct {
	Model      string     `json:"model"`
	Next       bool       `json:"next"`
	Held       bool       `json:"held"`
	HeldUntil  *time.Time `json:"held_until,omitempty"`
	LastPicked *time.Time `json:"last_picked,omitempty"`
}

// RotationMembers is the tier's pool on profile's row in pick order (least recent first, held last); read-only.
func RotationMembers(agentName, profile, tier string, now time.Time) []PoolMember {
	reg, err := providers.LoadShared()
	if err != nil {
		return nil
	}
	p := reg.Row(profile)
	if !router.RotationRow(p) {
		return nil
	}
	entries := router.ServedPool(p, rotationPool(agentName, cmp.Or(tierAliases[tier], models.Sonnet), p, now), now)
	e := router.LoadState()[router.AccountOf(p)]
	var live []string
	var out []PoolMember
	for _, entry := range entries {
		m := PoolMember{Model: providers.SpellPin(p, entry)}
		key := router.DiscoverySkipKey(agentName, entry)
		if at := e.Discovery[key]; !at.IsZero() {
			m.LastPicked = &at
		}
		m.Held, m.HeldUntil = heldUntil(e, key, entry, now)
		if !m.Held {
			live = append(live, entry)
		}
		out = append(out, m)
	}
	next := router.PeekRotation(agentName, router.AccountOf(p), p, live, now)
	for i := range out {
		out[i].Next = out[i].Model == next && next != ""
	}
	slices.SortStableFunc(out, func(a, b PoolMember) int {
		return cmp.Or(heldRank(a)-heldRank(b), timeOf(a.LastPicked).Compare(timeOf(b.LastPicked)))
	})
	return out
}

func heldRank(m PoolMember) int {
	if m.Held {
		return 1
	}
	return 0
}

// heldUntil is when a quota hold or a capability skip on entry ends; false when none is live.
func heldUntil(e router.Entry, key, entry string, now time.Time) (bool, *time.Time) {
	if until := e.ModelDead[router.ModelKey(entry)]; until.After(now) {
		return true, &until
	}
	if at, ok := e.DiscoverySkip[key]; ok && now.Sub(at) < router.DiscoverySkipTTL {
		until := at.Add(router.DiscoverySkipTTL)
		return true, &until
	}
	return false, nil
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// rotationPick is an asked auto's tier-pool pick on base's row; false (no pool, error) keeps the ranked pick.
func rotationPick(reg *providers.Registry, q ModelQuery, base ModelRoute, need autoNeed, c *AutoChoice, now time.Time) (ModelRoute, bool) {
	p := reg.Row(base.Profile)
	if !router.RotationRow(p) {
		return base, false
	}
	alias := cmp.Or(tierAliases[need.tier], models.Sonnet)
	stamp := q.ctx == nil || !peekingRotation(q.ctx)
	m, note, err := rotationModelOn([]*providers.Provider{p}, q.Agent, alias, stamp, now)
	if err != nil || m == "" {
		return base, false
	}
	pq := q
	pq.Model, pq.Profile, pq.Default = base.Profile+":"+m, "", ""
	r, err := ResolveModel(reg, pq, now)
	if err != nil {
		return base, false
	}
	r.Source, c.Model, c.Criterion = base.Source, r.Model, "rotation"
	r.Note = joinNotes(routeNote(base.Note), r.Note, note)
	r.Effort = autoEffort(r.Effort, q.Agent, need)
	return r, true
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
func RotationFreeze(ctx context.Context, agentName, brief, model string) (string, string) {
	if prefix, auto := SplitAuto(cmp.Or(model, agentModelPinContext(ctx, agentName))); !auto || prefix != "" {
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
