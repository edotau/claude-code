package router

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// Gates a candidate must pass before it is scored; a Skip names the one that dropped it.
const (
	gateRow       = "row"
	GateAvailable = "available"
	gateAccount   = "account"
	GateExhausted = "exhausted"
	gateFree      = "free" // a free opencode sub-candidate blocked at opus or on a work repo
	GateOff       = "off"  // the agent is deactivated (no profile set)
)

// Candidate is one runnable dispatch: the agent, its cell at the alias, its account's state and its composite score.
type Candidate struct {
	Agent string `json:"agent"`
	Cell
	State   string             `json:"state"`
	Billing string             `json:"billing"`
	Scores  map[string]float64 `json:"scores,omitempty"`
	Total   float64            `json:"total"`
	// Profile names the profile a pinned or session-anchored candidate runs on; empty for every other candidate.
	Profile string `json:"profile,omitempty"`
}

// Skip is an agent dropped by a gate, with why.
type Skip struct {
	Agent  string `json:"agent"`
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// PickResult is a pick: candidates in run order, the weights that scored them, then every skipped agent.
type PickResult struct {
	Alias      string             `json:"alias"`
	Want       string             `json:"want,omitempty"`
	Weights    map[string]float64 `json:"weights"`
	Candidates []Candidate        `json:"candidates"`
	Skipped    []Skip             `json:"skipped,omitempty"`
}

// PickOpts tunes a pick; nil funcs take the defaults (no presence check, no refresh, time.Now).
type PickOpts struct {
	Fallback   string
	Configured func(agent string, c Cell) error
	// Account resolves the backend an agent actually uses ("" keeps the table's account key) and its billing type.
	Account func(agent string, c Cell) (account, billing string)
	// Refresh re-fetches one stale account and records it; the pick then rereads that entry.
	Refresh func(ctx context.Context, account string) bool
	Now     func() time.Time
	// WorkRepo blocks free Zen on work repos; nil checks the cwd via IsWorkRepo.
	WorkRepo func() bool
	// Pins is every agent's routing pins, read once by the caller (agent.AgentPins).
	Pins map[string]PinSpec
	// CanServe reports whether provider p is usable by agent name (the roster's Serves).
	CanServe func(agent string, p *providers.Provider) bool
	// CodexModel respells codex's Databricks target for the agent (the pin or flag rule).
	CodexModel func(providers.Target) providers.Target
	// Lead is the profile the wanted agent leads with (coworkers: the session's or an override); "" = its pin leads.
	Lead string
	dry  bool // refreshStale's collecting pass: must write nothing (no Discovery rotation)
}

// canServe is CanServe, or true for every pair when unset.
func (o PickOpts) canServe(agent string, p *providers.Provider) bool {
	return o.CanServe == nil || o.CanServe(agent, p)
}

// codexModel is CodexModel applied to t, or t itself when unset.
func (o PickOpts) codexModel(t providers.Target) providers.Target {
	if o.CodexModel == nil {
		return t
	}
	return o.CodexModel(t)
}

// modelPin is model, except a dry pass drops a Discovery pin: only the real pick advances the rotation.
func (o PickOpts) modelPin(model string) string {
	if o.dry && model == DiscoveryModel {
		return ""
	}
	return model
}

// PinSpec is one agent's routing pins: its profile, its model pin, and whether it is switched off.
type PinSpec struct {
	Profile, Model string
	Off            bool
}

// pickScope is what a Dimension may read: the account states and every candidate scored together.
type pickScope struct {
	State      State
	Candidates []Candidate
}

// Dimension is one scored axis, 0–1 with 1 better; success rate, latency or alias fit can be added beside these.
type Dimension interface {
	Name() string
	Score(c Candidate, s pickScope) float64
}

// Dimensions are the scored axes; table.json's weights must name exactly these.
var Dimensions = []Dimension{priceDim{}, headroomDim{}}

// Pick gates/scores want first, then ladder/score (low after non-low); FallbackAll Claude is last unless placed.
func (t *RouteTable) Pick(ctx context.Context, alias, want string, st State, o PickOpts) PickResult {
	now, configured := time.Now(), o.Configured
	if o.Now != nil {
		now = o.Now()
	}
	if configured == nil {
		configured = func(string, Cell) error { return nil }
	}
	if st == nil {
		st = State{}
	}
	if o.Refresh != nil {
		o.Refresh = t.refreshStale(ctx, alias, want, st, o, now, configured)
	}
	return t.pick(ctx, alias, want, st, o, now, configured)
}

// pick is Pick after the stale accounts are refreshed.
func (t *RouteTable) pick(
	ctx context.Context, alias, want string, st State, o PickOpts, now time.Time, configured func(agent string, c Cell) error,
) PickResult {
	res := PickResult{Alias: alias, Want: want, Weights: t.Weights}
	pins := o.Pins
	reg, err := providers.Load()
	if err != nil {
		res.Skipped = append(res.Skipped, Skip{"registry", gateRow, "provider registry did not load, profile pins unchecked: " + err.Error()})
	}
	pg := &pickGate{reg: reg, held: map[string]bool{}}
	gate := func(agent string) []Candidate {
		return t.gateCandidate(ctx, pg, alias, agent, pins, st, o, now, configured, &res)
	}
	firstG, restG, lastG := t.gatherCandidates(want, o, gate)
	lp := ladderPick{t: t, reg: reg, l: t.ladderFor(), alias: alias, now: now, res: &res}
	firstG = lp.admitGroups(firstG)
	lastG = lp.admitGroups(lastG)
	restG, lastG = lp.l.claim(restG, lastG)
	firstG, restG = lp.l.armSplit(firstG, restG)
	units := lp.units(restG)
	restG = make([][]Candidate, len(units))
	for i, u := range units {
		restG[i] = u.cs
	}
	flatFirst, flatRest, flatLast := slices.Concat(firstG...), slices.Concat(restG...), slices.Concat(lastG...)
	all := t.score(slices.Concat(flatFirst, flatRest, flatLast), st, now)
	nf, nr := len(flatFirst), len(flatRest)
	firstG = regroup(firstG, all[:nf])
	for i, g := range regroup(restG, all[nf:nf+nr]) {
		units[i].cs = g
	}
	lastG = regroup(lastG, all[nf+nr:])
	// units sort whole (not their individual candidates), so a free winner keeps its paid backup adjacent.
	lp.l.placeClaude(units)
	sortUnits(units)
	res.Candidates = slices.Concat(firstG...)
	for _, u := range units {
		res.Candidates = append(res.Candidates, u.cs...)
	}
	res.Candidates = append(res.Candidates, slices.Concat(lastG...)...)
	res.Candidates = deadLast(dedupeCandidates(res.Candidates), st, now, &res)
	return res
}

// deadLast moves candidates whose model carries a live quota-dead mark behind the rest, in order, noting each.
func deadLast(cs []Candidate, st State, now time.Time, res *PickResult) []Candidate {
	var live, dead []Candidate
	for _, c := range cs {
		if !st.accountModelDead(c.Account, c.Model, now) {
			live = append(live, c)
			continue
		}
		dead = append(dead, c)
		res.Skipped = append(res.Skipped, Skip{c.Agent, GateExhausted, c.Model + " is quota-denied on " + c.Account + "; tried last"})
	}
	return append(live, dead...)
}

// refreshConcurrency bounds the stale-account refreshes one pick runs at once.
const refreshConcurrency = 4

// refreshStale dry-picks stale accounts, refreshes concurrently, and prevents repeat refreshes in the real pick.
func (t *RouteTable) refreshStale(
	ctx context.Context, alias, want string, st State, o PickOpts, now time.Time, configured func(agent string, c Cell) error,
) func(context.Context, string) bool {
	refresh := o.Refresh
	var stale []string
	o.dry = true
	o.Refresh = func(_ context.Context, account string) bool {
		if !slices.Contains(stale, account) {
			stale = append(stale, account)
		}
		return false
	}
	t.pick(ctx, alias, want, st.clone(), o, now, configured)
	var wg sync.WaitGroup
	sem := make(chan struct{}, refreshConcurrency)
	for _, account := range stale {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			refresh(ctx, account)
		})
	}
	wg.Wait()
	if len(stale) > 0 {
		fresh := LoadState()
		for _, account := range stale {
			if e, ok := fresh[account]; ok {
				st[account] = e
			}
		}
	}
	return func(ctx context.Context, account string) bool {
		return !slices.Contains(stale, account) && refresh(ctx, account)
	}
}

// dedupeCandidates drops later repeats of the same agent, account and model, keeping order.
func dedupeCandidates(cs []Candidate) []Candidate {
	seen := make(map[[3]string]bool)
	return slices.DeleteFunc(cs, func(c Candidate) bool {
		key := [3]string{c.Agent, c.Account, c.Model}
		if seen[key] {
			return true
		}
		seen[key] = true
		return false
	})
}

// OffOrder reports a gate skip of an agent the order never holds: not the wanted agent, claude or the registry.
func OffOrder(label, want string) bool {
	who, _, _ := strings.Cut(label, "·")
	return who != want && who != "claude" && who != "registry"
}

// ClaudeBackstop is the last claude candidate when the fallback mode runs it after the others; -1 = none.
func ClaudeBackstop(cands []Candidate, req int, mode string) int {
	if mode != FallbackAll && mode != "" {
		return -1
	}
	last := -1
	for i, c := range cands {
		if c.Agent == "claude" {
			last = i
		}
	}
	if last == req {
		return -1
	}
	return last
}

// RequestedIndex is the candidate the caller asked for: the wanted agent on the overlay workspace, the lead or its pin.
func RequestedIndex(cands []Candidate, want, lead string, pin int) int {
	on := func(profile string) int {
		acct := ProviderAccount(profile)
		return slices.IndexFunc(cands, func(c Candidate) bool {
			return c.Agent == want && (c.Profile == profile || c.Account == acct)
		})
	}
	switch arm := ArmProfile(); {
	case arm != "":
		return on(arm)
	case want == "claude":
		return slices.IndexFunc(cands, func(c Candidate) bool { return c.Agent == "claude" })
	case lead != "":
		return on(lead)
	}
	return pin
}

// AskedWorkspace reports the wanted agent on the overlay workspace or inside a confinement: neither is a fallback.
func AskedWorkspace(cand Candidate, want string) bool {
	if cand.Agent != want {
		return false
	}
	if arm := ArmProfile(); arm != "" && cand.Profile == arm {
		return true // databricks:<p> asked for this workspace: running there is not a fallback
	}
	cp, _ := Confined() // the confinement admitted it: the pin was set aside, nothing fell back
	return cp != ""
}

// skipNoRow records that agent has no row at alias.
func skipNoRow(res *PickResult, agent, alias string) {
	res.Skipped = append(res.Skipped, Skip{agent, gateRow, "no " + alias + " row"})
}

// skipNotConfigured records that agent failed its configured check with err.
func skipNotConfigured(res *PickResult, agent string, err error) {
	res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "not configured: " + textutil.FirstLine(strings.TrimSpace(err.Error()))})
}

// regroup reslices a flat, scored candidate list back into groups sized like the originals.
func regroup(groups [][]Candidate, flat []Candidate) [][]Candidate {
	out := make([][]Candidate, len(groups))
	for i, g := range groups {
		out[i], flat = flat[:len(g)], flat[len(g):]
	}
	return out
}

// gateAgent records failed gates; returns one candidate, or opencode’s free winner plus its paid backup.
func (t *RouteTable) gateAgent(
	ctx context.Context, alias, agent string, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if agent == "opencode" && !t.OpencodeRowOverridden[alias] {
		return t.gateOpencodeCandidate(ctx, alias, "", st, o, now, configured, res)
	}
	c, ok := t.Cell(agent, alias)
	if !ok {
		skipNoRow(res, agent, alias)
		return nil
	}
	if err := configured(agent, c); err != nil {
		skipNotConfigured(res, agent, err)
		return nil
	}
	billing := t.Billing[c.Account]
	if o.Account != nil {
		if a, b := o.Account(agent, c); a != "" {
			c.Account, billing = a, b
		}
	}
	cand, ok := t.gateAccount(ctx, agent, c, billing, st, o, now, res)
	if !ok {
		return nil
	}
	return []Candidate{cand}
}

// gateAccount runs a resolved cell through the copilot-pin, auth-mark, refresh and exhausted gates; name labels Skips.
func (t *RouteTable) gateAccount(
	ctx context.Context, name string, c Cell, billing string, st State, o PickOpts, now time.Time, res *PickResult,
) (Candidate, bool) {
	if c.Account == copilotUnpinned {
		res.Skipped = append(res.Skipped, Skip{name, gateAccount, "no pinned Copilot account (claude-code configure github)"})
		return Candidate{}, false
	}
	if reason, marked := st.accountAuthMarked(c.Account, now); marked {
		res.Skipped = append(res.Skipped, Skip{name, GateAuth, authSkipReason(reason, c.Account)})
		return Candidate{}, false
	}
	e := st[c.Account]
	if o.Refresh != nil && e.Stale(now) && e.Status(now) != StateExhausted && o.Refresh(ctx, c.Account) {
		e = LoadState()[c.Account]
		st[c.Account] = e
	}
	s := e.Status(now)
	if s == StateExhausted {
		res.Skipped = append(res.Skipped, Skip{name, GateExhausted, ExhaustedReason(c.Account, e, now)})
		return Candidate{}, false
	}
	return Candidate{Agent: accountProvider(name), Cell: c, State: s, Billing: billing}, true
}

// opencodeOrder is free Zen then configured paid order; scores only break ties at the same position (owner policy).
func (t *RouteTable) opencodeOrder() []string {
	return slices.Concat([]string{"zen-free"}, t.OpencodeLadder)
}

// gateOpencodeCandidate gates logins/quota/free rules in order; a free winner keeps its first paid backup adjacent.
func (t *RouteTable) gateOpencodeCandidate(
	ctx context.Context, alias, only string, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	var ok []opencodeSub
	order := t.opencodeOrder()
	if only != "" {
		order = []string{only}
	}
	for _, id := range order {
		if s, pass := t.gateOpencodeSub(ctx, id, alias, st, o, now, configured, res); pass {
			ok = append(ok, s)
		}
	}
	if len(ok) == 0 {
		res.Skipped = append(res.Skipped, Skip{"opencode", GateAvailable, "no opencode sub-candidate passed (no login, exhausted, or free blocked)"})
		return nil
	}
	slices.SortStableFunc(ok, func(a, b opencodeSub) int { return cmp.Compare(lowRank(a.state), lowRank(b.state)) })
	w := ok[0]
	group := []Candidate{{Agent: "opencode", Cell: w.c, State: w.state, Billing: w.billing}}
	if only != "" {
		return group
	}
	if w.billing == BillFree {
		for _, s := range ok[1:] {
			if s.billing != BillFree {
				group = append(group, Candidate{Agent: "opencode", Cell: s.c, State: s.state, Billing: s.billing})
				break
			}
		}
	}
	return group
}

// opencodeSub is one opencode ladder entry that cleared every gate.
type opencodeSub struct {
	id      string
	c       Cell
	billing string
	state   string
}

// gateOpencodeSub gates one ladder id at alias; pass is false when it is unknown, absent, blocked or unavailable.
func (t *RouteTable) gateOpencodeSub(
	ctx context.Context, id, alias string, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) (opencodeSub, bool) {
	workRepo := o.WorkRepo
	if workRepo == nil {
		workRepo = func() bool { return IsWorkRepo("") }
	}
	name := "opencode·" + id
	if _, known := t.OpencodeSub[id]; !known {
		res.Skipped = append(res.Skipped, Skip{name, gateRow, "unknown opencode ladder id"})
		return opencodeSub{}, false
	}
	c, has := t.OpencodeSub[id][alias]
	if !has {
		return opencodeSub{}, false // e.g. zen-free carries no "opus" row — that is how it stays out of opus, not an alias check
	}
	billing := t.Billing[c.Account]
	if billing == BillFree && workRepo() {
		res.Skipped = append(res.Skipped, Skip{name, gateFree, "free blocked on a work repo"})
		return opencodeSub{}, false
	}
	if billing == BillFree && !t.OpencodeRouting.FreeTierOK(alias) {
		res.Skipped = append(res.Skipped, Skip{name, gateFree, "free not enabled at " + alias})
		return opencodeSub{}, false
	}
	if err := configured("opencode", c); err != nil {
		skipNotConfigured(res, name, err)
		return opencodeSub{}, false
	}
	cand, pass := t.gateAccount(ctx, name, c, billing, st, o, now, res)
	if !pass {
		return opencodeSub{}, false
	}
	return opencodeSub{id, c, billing, cand.State}, true
}

// gatherCandidates gates want, other agents, then FallbackAll Claude; free+paid groups remain inseparable.
func (t *RouteTable) gatherCandidates(want string, o PickOpts, gate func(string) []Candidate) (first, rest, last [][]Candidate) {
	if want != "" {
		if cs := gate(want); len(cs) > 0 {
			first = append(first, cs)
		}
	}
	if o.Fallback == FallbackNone || (want == "claude" && ArmProfile() == "") { // the route falls a wanted claude through
		return first, nil, nil
	}
	rest = gateOthers(want, gate)
	if want != "claude" {
		last = claudeLast(o, gate)
	}
	return first, rest, last
}

// gateOthers gates every table agent but want and claude, in table order.
func gateOthers(want string, gate func(string) []Candidate) (rest [][]Candidate) {
	for _, r := range TableAgents {
		if r == want || r == "claude" {
			continue
		}
		if cs := gate(r); len(cs) > 0 {
			rest = append(rest, cs)
		}
	}
	return rest
}

// claudeLast gates claude as the final fallback under FallbackAll (or the unset default).
func claudeLast(o PickOpts, gate func(string) []Candidate) [][]Candidate {
	if o.Fallback != FallbackAll && o.Fallback != "" {
		return nil
	}
	if cs := gate("claude"); len(cs) > 0 {
		return [][]Candidate{cs}
	}
	return nil
}

// score fills each candidate's per-dimension scores and weighted total, less coolingPenalty on a cooling account.
func (t *RouteTable) score(cs []Candidate, st State, now time.Time) []Candidate {
	sc := pickScope{State: st, Candidates: cs}
	out := make([]Candidate, len(cs))
	for i, c := range cs {
		c.Scores = map[string]float64{}
		for _, d := range Dimensions {
			v := d.Score(c, sc)
			c.Scores[d.Name()] = v
			c.Total += t.Weights[d.Name()] * v
		}
		if st.accountCooling(c.Account, now) {
			c.Total -= coolingPenalty
		}
		out[i] = c
	}
	return out
}

func lowRank(s string) int {
	if s == StateLow {
		return 1
	}
	return 0
}

// priceDim: a subscription window already paid for scores its share left; credits score cheapest-at-tier = 1.
type priceDim struct{}

func (priceDim) Name() string { return "price" }

func (priceDim) Score(c Candidate, s pickScope) float64 {
	if c.Billing == BillFree {
		return 1
	}
	if c.Billing == BillSubscription {
		return headroom(s.State[c.Account])
	}
	p, low := blended(c.Price), -1.0
	for _, o := range s.Candidates {
		if b := blended(o.Price); o.Billing != BillSubscription && b > 0 && (low < 0 || b < low) {
			low = b
		}
	}
	if p <= 0 || low <= 0 {
		return 1 // unpriced cells tie
	}
	return low / p
}

// headroomDim is 1 − the used share of the account's tightest window.
type headroomDim struct{}

func (headroomDim) Name() string { return "headroom" }

func (headroomDim) Score(c Candidate, s pickScope) float64 { return headroom(s.State[c.Account]) }

// headroom of an unseen account is 0.5: unknown ranks below a reading known to have room.
func headroom(e Entry) float64 {
	if e.Seen.IsZero() {
		return 0.5
	}
	return min(max(1-e.UsedPct/100, 0), 1)
}

// blended weights input 3:1 over output, the usual agentic read/write mix.
func blended(p Price) float64 { return (3*p.In + p.Out) / 4 }

// authSkipReason is the mark's own reason, else the login hint for the account's provider.
func authSkipReason(reason, account string) string {
	if reason != "" {
		return reason
	}
	return "auth rejected: run `claude-code databricks login --profile " + strings.TrimPrefix(account, "databricks·") + "`"
}

// ExhaustedReason names the account and its reset, e.g. "github·aueric_boehcp exhausted until 2026-11-01".
func ExhaustedReason(account string, e Entry, now time.Time) string {
	s := account + " exhausted"
	if u := e.Until(now); !u.IsZero() {
		layout := "2006-01-02 15:04"
		if u.Sub(now) > 24*time.Hour {
			layout = "2006-01-02"
		}
		s += " until " + u.Local().Format(layout)
	}
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	return s
}

// Fallback modes for an unavailable dispatch: the full chain ending in claude, other agents only, or none.
const (
	FallbackAll    = "all"
	FallbackAgents = "agents"
	FallbackNone   = "none"
)

// FallbackModes lists the accepted --fallback values.
var FallbackModes = []string{FallbackAll, FallbackAgents, FallbackNone}

// EnvFallback set (process or vault env) means a failed dispatch falls back; unset means it exits on the error.
const EnvFallback = "AGENT_ERROR_FALLBACK"

// DefaultFallback is FallbackAll when EnvFallback is set non-empty (process env, else vault env), else FallbackNone.
func DefaultFallback() string {
	if strings.TrimSpace(os.Getenv(EnvFallback)) != "" || strings.TrimSpace(providers.CustomEnv()[EnvFallback]) != "" {
		return FallbackAll
	}
	return FallbackNone
}

// gitTimeout bounds each git call so a hung one fails closed instead of blocking a pick.
const gitTimeout = 10 * time.Second

// workRepoMarkers loads case-insensitive remote substrings; empty means personal, errors fail closed in IsWorkRepo.
var workRepoMarkers = func() ([]string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	return reg.Routing.Opencode.Markers(), nil
}

// gitRemoteURLs lists fetch/push URLs via git remote -v; tests replace this seam.
var gitRemoteURLs = perDir(func(dir string) ([]string, error) {
	if dir == "" {
		dir = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "-v").Output()
	if err != nil {
		return nil, err
	}
	var urls []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			urls = append(urls, fields[1])
		}
	}
	return urls, nil
})

// perDir memoizes f's successful answers by absolute dir for the process; errors are retried, never cached.
func perDir[T any](f func(string) (T, error)) func(string) (T, error) {
	var cache sync.Map
	return func(dir string) (T, error) {
		key, err := filepath.Abs(dir)
		if err != nil {
			return f(dir)
		}
		if v, ok := cache.Load(key); ok {
			return v.(T), nil
		}
		v, err := f(dir)
		if err == nil {
			cache.Store(key, v)
		}
		return v, err
	}
}

// gitInRepo runs `git rev-parse --git-dir`: only git's "not a git repository" verdict is false; other failures error.
var gitInRepo = perDir(func(dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir")
	cmd.Env = append(os.Environ(), "LC_ALL=C") // pin git's message language for the stderr match
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && strings.Contains(stderr.String(), "not a git repository") {
		return false, nil
	}
	return false, err
})

// IsWorkRepo matches remote markers; empty markers/non-repos are personal, registry or git errors fail closed.
func IsWorkRepo(dir string) bool {
	markers, err := workRepoMarkers()
	if err != nil {
		return true // registry unreadable: fail closed, never fail open on free
	}
	if len(markers) == 0 {
		return false
	}
	if dir == "" {
		dir = "."
	}
	inRepo, err := gitInRepo(dir)
	if err != nil {
		return true // git missing or errored (e.g. corrupt config): fail closed
	}
	if !inRepo {
		return false
	}
	urls, err := gitRemoteURLs(dir)
	if err != nil {
		return true // git errored inside a repo: fail closed
	}
	return matchesWorkMarkers(urls, markers)
}

func matchesWorkMarkers(urls, markers []string) bool {
	for _, u := range urls {
		u = strings.ToLower(u)
		for _, m := range markers {
			if strings.Contains(u, strings.ToLower(m)) {
				return true
			}
		}
	}
	return false
}
