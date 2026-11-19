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

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

func init() { router.RotationPool = rotationPool }

type peekKey struct{}

// PeekContext marks ctx's route resolution as a label: a rotation pick is peeked, never recorded.
func PeekContext(ctx context.Context) context.Context { return context.WithValue(ctx, peekKey{}, true) }

// Peeking reports whether ctx carries PeekContext's mark.
func Peeking(ctx context.Context) bool { v, _ := ctx.Value(peekKey{}).(bool); return v }

// rotationTiers maps a route alias to the ranks.json rotation tier whose floor and cost cap it takes.
var rotationTiers = map[string]string{models.Haiku: "fast", models.Sonnet: "standard", models.Opus: "deep"}

// rotationPool is the served models agent runs on p that meet alias's rotation rule; "" alias is every tier's union.
func rotationPool(agentName, alias string, p *providers.Provider, tools bool, now time.Time) []string {
	reg, err := providers.LoadShared()
	if err != nil || !router.RotationRow(p) {
		return nil
	}
	cands := rotationCandidates(reg, agentName, p, tools, now)
	tiers := []string{"fast", "standard", "deep"}
	if t := rotationTiers[alias]; t != "" {
		tiers = []string{t}
	}
	var out []string
	for _, t := range tiers {
		for _, m := range catalog.RotationPool(cands, t, tools) {
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
func rotationCandidates(reg *providers.Registry, agentName string, p *providers.Provider, tools bool, now time.Time) []catalog.Candidate {
	if p.Preset == providers.PresetDatabricks {
		menu := providers.OpencodeMenu()
		cands, _ := autoCandidates(reg, ModelQuery{Agent: agentName, Tools: tools, menu: &menu}, p, now)
		if agentName == agentid.OpenCode {
			return opencodeRunnable(p, cands)
		}
		return cands
	}
	if router.GithubCatalogIDs == nil {
		return nil
	}
	var out []catalog.Candidate
	var probes map[string]serving.CheckResult
	if tools {
		probes, _ = catalog.LoadCheckCache(p.Name)
	}
	for _, id := range router.GithubCatalogIDs(p) {
		if catalog.ID(id) != AutoModel { // Copilot's own auto router is no model to rotate onto
			out = append(out, catalog.Candidate{Model: id, Tools: probes[catalog.ID(id)].Tools})
		}
	}
	return out
}

// opencodeRunnable keeps candidates opencode lists on p (base menu or a family id) with a route for the wire.
func opencodeRunnable(p *providers.Provider, cands []catalog.Candidate) []catalog.Candidate {
	listed := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		listed[agentconfig.MenuWire(p, m.ID)] = true
	}
	return slices.DeleteFunc(cands, func(c catalog.Candidate) bool {
		_, _, member := providers.FamilyVersion(c.Model)
		_, routed, _ := providers.WireFor(p, c.Model)
		return !listed[c.Model] && !member || !routed
	})
}

// rotatesUnderAuto is false for claude: an asked auto keeps the brief's tier model (D7 b), so label and run agree.
func rotatesUnderAuto(agentName string) bool { return !VaultAgentIsClaude(agentName) }

// untieredAuto is an asked auto's alias for a brief with no tier: opus for a rotating agent, else sonnet.
func untieredAuto(agentName string) string {
	if rotatesUnderAuto(agentName) {
		return models.Opus
	}
	return models.Sonnet
}

// rotationRows are the rows agent rotates on: every serving workspace under the pool pin, else its pinned row.
func rotationRows(reg *providers.Registry, agentName string) []*providers.Provider {
	if !rotatesUnderAuto(agentName) {
		return nil
	}
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
	return rotationModelOn(rotationRows(reg, agentName), agentName, alias, stamp, false, now)
}

// rotationModelOn is RotationModel over the given rows.
func rotationModelOn(rows []*providers.Provider, agentName, alias string, stamp, tools bool, now time.Time) (string, string, error) {
	if len(rows) == 0 {
		return "", "", nil
	}
	st := router.LoadState()
	var pool []string
	for _, p := range rows {
		for _, m := range rotationPool(agentName, alias, p, tools, now) {
			// skips and holds live on the workspace's account, not the pool account the pick below reads
			held, _ := heldUntil(st[router.AccountOf(p)], router.DiscoverySkipKey(agentName, m), m, now)
			if !slices.Contains(pool, m) && (len(rows) == 1 || !held) {
				pool = append(pool, m)
			}
		}
	}
	account, p := router.AccountOf(rows[0]), rows[0]
	if len(rows) > 1 { // the pool pin: one history for every workspace, entries already wire ids
		account, p = router.ProviderAccount(PoolPin), nil
	}
	hist := account
	if rows[0].Preset == providers.PresetDatabricks {
		hist = router.ProviderAccount(PoolPin)
	}
	n := takeable(st[account], agentName, p, pool, now)
	if !stamp {
		m := router.PeekRotation(agentName, account, hist, p, pool, now)
		return m, rotationNote(m, alias, n, "next"), nil
	}
	m, err := router.PickRotation(agentName, account, hist, p, pool, now)
	return m, rotationNote(m, alias, n, "picked"), err
}

// takeable counts pool entries a pick could take: served and not on a capability skip.
func takeable(e router.Entry, agentName string, p *providers.Provider, pool []string, now time.Time) int {
	if p != nil {
		pool = router.ServedPool(p, pool, now)
	}
	return len(pool) - len(slices.DeleteFunc(slices.Clone(pool), func(m string) bool {
		held, _ := heldUntil(e, router.DiscoverySkipKey(agentName, m), m, now)
		return !held
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
	entries := router.ServedPool(p, rotationPool(agentName, cmp.Or(tierAliases[tier], models.Sonnet), p, false, now), now)
	st := router.LoadState()
	account, hist := router.AccountOf(p), router.AccountOf(p)
	if p.Preset == providers.PresetDatabricks {
		hist = router.ProviderAccount(PoolPin)
	}
	e, history := st[account], st[hist]
	var live []string
	var out []PoolMember
	for _, entry := range entries {
		m := PoolMember{Model: providers.SpellPin(p, entry)}
		key := router.DiscoverySkipKey(agentName, entry)
		if at := history.Discovery[key]; !at.IsZero() {
			m.LastPicked = &at
		}
		m.Held, m.HeldUntil = heldUntil(e, key, entry, now)
		if !m.Held {
			live = append(live, entry)
		}
		out = append(out, m)
	}
	next := router.PeekRotation(agentName, account, hist, p, live, now)
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

// ErrPoolExhausted marks a fresh rotation pool with no eligible model.
var ErrPoolExhausted = fmt.Errorf("%w: auto pool exhausted", ErrModelRoute)

type autoStopError struct{ error }

func (e autoStopError) Unwrap() error { return e.error }

func freshRotation(p *providers.Provider, now time.Time) bool {
	if !router.RotationRow(p) {
		return false
	}
	if p.Preset == providers.PresetDatabricks {
		listing, _ := router.LoadServed(p.Name)
		return listing.Fresh(now)
	}
	return router.GithubCatalogTime != nil && now.Sub(router.GithubCatalogTime(p)) < serving.CacheTTL()
}

// rotationPick keeps a cold listing on the tier fallback and refuses an exhausted fresh pool.
func rotationPick(reg *providers.Registry, q ModelQuery, base ModelRoute, need autoNeed, c *AutoChoice, now time.Time) (ModelRoute, bool, error) {
	p := reg.Row(base.Profile)
	if !freshRotation(p, now) {
		return base, false, nil
	}
	if !rotatesUnderAuto(q.Agent) {
		return autoFallback(reg, q, base, need, c, q.Agent+" keeps its tier model under auto", now), true, nil
	}
	m, note, alias, err := tierRotation(q, cmp.Or(tierAliases[need.tier], models.Sonnet), p, now)
	if err != nil {
		return base, true, autoStopError{fmt.Errorf("auto pick state: %w", err)}
	}
	if m == "" {
		return base, true, exhaustedPool(q.Agent, alias, p, q.Tools, now)
	}
	pq := q
	pq.Model, pq.Profile, pq.Default = base.Profile+":"+m, "", ""
	r, err := ResolveModel(reg, pq, now)
	if err != nil {
		return base, true, err
	}
	r.Source, c.Model, c.Criterion = base.Source, r.Model, "rotation"
	r.Note = joinNotes(routeNote(base.Note), r.Note, note)
	r.Effort = autoEffort(r.Effort, q.Agent, need)
	return r, true, nil
}

// tierRotation is the pick at alias on p, stepping to standard when the row serves nothing in alias's tier.
func tierRotation(q ModelQuery, alias string, p *providers.Provider, now time.Time) (string, string, string, error) {
	stamp := q.ctx == nil || !Peeking(q.ctx)
	m, note, err := rotationModelOn([]*providers.Provider{p}, q.Agent, alias, stamp, q.Tools, now)
	if err == nil && m == "" && alias != models.Sonnet && len(heldIn(q.Agent, alias, p, q.Tools, now)) == 0 {
		alias = models.Sonnet // the row serves nothing in this tier: step to standard, as the tier ladder does
		m, note, err = rotationModelOn([]*providers.Provider{p}, q.Agent, alias, stamp, q.Tools, now)
	}
	return m, note, alias, err
}

func exhaustedPool(agentName, alias string, p *providers.Provider, tools bool, now time.Time) error {
	held := heldIn(agentName, alias, p, tools, now)
	return fmt.Errorf("%w on %s; held ids: %s", ErrPoolExhausted, p.Name, cmp.Or(strings.Join(held, ", "), "none"))
}

// heldIn is the pool ids of alias's tier on p that a skip or hold keeps out of the pick.
func heldIn(agentName, alias string, p *providers.Provider, tools bool, now time.Time) []string {
	e := router.LoadState()[router.AccountOf(p)]
	var held []string
	for _, id := range rotationPool(agentName, alias, p, tools, now) {
		if yes, _ := heldUntil(e, router.DiscoverySkipKey(agentName, id), id, now); yes {
			held = append(held, id)
		}
	}
	return held
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
			n = max(n, len(rotationPool(agentName, alias, p, false, now)))
		}
		sizes = append(sizes, fmt.Sprintf("%s %d", rotationTiers[alias], n))
	}
	return "rotating " + strings.Join(sizes, ", ") + "; 7d " + recentPicks(agentName, now)
}

// recentPicks is agent's last 7 days of runs counted by model, top four: "kimi-k3 3, gpt-6-2-sol 2".
func recentPicks(agentName string, now time.Time) string {
	counts := map[string]int{}
	routes := map[string]int{}
	_ = eachRecent(paths.DispatchDir(), jsonToken(agentName), func(r Record) bool { // an unreadable dir lists none
		m := cmp.Or(r.Invoked, r.Model)
		if r.Agent != agentName || now.Sub(r.Time) >= 7*24*time.Hour {
			return true
		}
		if r.RanRoute != "" {
			routes[strings.TrimPrefix(r.RanRoute, "databricks:")]++
		}
		if m == "" {
			return true
		}
		if _, after, ok := strings.Cut(m, ":"); ok {
			m = after
		}
		counts[catalog.ID(m)]++
		return true
	})
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	var out []string
	for _, k := range keys[:min(len(keys), 4)] {
		out = append(out, fmt.Sprintf("%s %d", k, counts[k]))
	}
	summary := cmp.Or(strings.Join(out, ", "), "no runs")
	var routeCounts []string
	for _, route := range slices.Sorted(maps.Keys(routes)) {
		routeCounts = append(routeCounts, fmt.Sprintf("%s %d", route, routes[route]))
	}
	if len(routeCounts) > 0 {
		summary += "; routes " + strings.Join(routeCounts, ", ")
	}
	return summary
}
