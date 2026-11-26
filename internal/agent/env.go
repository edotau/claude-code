// env.go — the one env builder for every headless agent child: ChildCmd, claude's launch, the Copilot SDK.
package agent

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/rtkwrap"
	"github.com/edotau/claude-code/internal/remote/network/remote"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// EnvMode picks the deny scrub a child's inherited env goes through.
type EnvMode int

const (
	EnvClaude      EnvMode = iota // proc.Scrub: Claude Code keeps HARNESS_VAULT_KEY so its hooks can open the vault
	EnvAgent                      // proc.ScrubAgent: codex, copilot, gemini, opencode children
	EnvPrescrubbed                // Base already went through a deny scrub (launch Plan.Env): isolation only
)

// EnvSpec is a headless child's env: Base scrubbed per Mode and batch isolation, then Set, Extra and PWD.
type EnvSpec struct {
	Mode  EnvMode
	Base  []string // nil means os.Environ(), except under EnvPrescrubbed where it is used as given
	Keep  []string // names both scrubs keep (--keep-env); the deny scrub also honors HARNESS_KEEP_ENV
	Unset []string // names dropped from the scrubbed base (Plan.Unset, an overlay unset); Set and Extra re-add them
	Set   []string // the launch's own KEY=VALUE, after the scrubs so they are never dropped
	Extra []string // --env, after Set so it wins
	PWD   string   // non-empty appends PWD=<dir> last, for a child that reads $PWD
	Agent string   // the launched agent: its own login names survive the EnvAgent credential scrub ("" = none)
}

// Environ builds the child env; ctx carries batch isolation and the hop's own provider (hostguard.go).
func (s EnvSpec) Environ(ctx context.Context) []string {
	ctx = withLaunchAgent(ctx, s.Agent)
	base := s.Base
	if base == nil && s.Mode != EnvPrescrubbed {
		base = runEnviron(ctx)
	}
	base, _ = s.Mode.scrub(ctx, base, s.Keep)
	base = withoutCarried(ctx, base)
	base = withoutShimPath(base) // only Pi's own launch (Set) puts the rtk shim dir on PATH
	base = withToolchainPath(base)
	drop := s.dropper(ctx)
	env := slices.DeleteFunc(slices.Clone(scrubIsolated(ctx, base, s.Keep)), drop)
	child := confinementOverlay(ctx)
	env = withOverlayDoc(env, childDoc(child), drop)
	// a coworker hop or probe drops a launch-set foreign login too (opencode's LaunchEnv adds OPENCODE_API_KEY everywhere)
	env = append(append(env, dropForeignLogins(ctx, s.Set, s.Keep)...), s.Extra...)
	if id, ok := ctx.Value(dispatchIDKey{}).(string); ok {
		env = s.dispatchMarks(ctx, env, id)
	}
	env = withOverlayCodemode(ctx, env, child.Codemode)
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return withMemoryOrigin(withRTKQuiet(env))
}

// withOverlayDoc adds the child's overlay unless a drop rule names it: one hop, only the confinement goes down.
func withOverlayDoc(env []string, doc string, drop func(string) bool) []string {
	if doc == "" || !drop(route.OverlayVar+"=") {
		return env
	}
	return append(env, route.OverlayVar+"="+doc)
}

// withoutCarried is base without the env a dispatch child gets from its overlay copy instead (proc.DispatchCarried).
func withoutCarried(ctx context.Context, base []string) []string {
	if !inDispatch(ctx) {
		return base
	}
	return proc.Drop(base, nil, proc.DispatchCarried)
}

// withOverlayCodemode sets a dispatch child's CODEMODE from its overlay copy, unless the launch already set one.
func withOverlayCodemode(ctx context.Context, env []string, codemode string) []string {
	if codemode == "" || !inDispatch(ctx) || slices.ContainsFunc(env, func(e string) bool { return proc.EnvHasPrefix(e, rtkwrap.CodemodeEnv+"=") }) {
		return env
	}
	return append(env, rtkwrap.CodemodeEnv+"="+codemode) // hooks and shims read it from the env
}

// dispatchMarks is env for a dispatch child: its id and depth and its proxy scope.
func (s EnvSpec) dispatchMarks(ctx context.Context, env []string, id string) []string {
	// the ids mark the child so `agents cancel --lost` can find it once orphaned
	depth, _ := ctx.Value(dispatchDepthKey{}).(int)
	env = append(slices.DeleteFunc(env, func(e string) bool {
		return proc.EnvHasPrefix(e, proc.DispatchIDEnv+"=") || proc.EnvHasPrefix(e, proc.DispatchDepthEnv+"=")
	}), proc.DispatchIDEnv+"="+id, proc.DispatchDepthEnv+"="+strconv.Itoa(depth))
	return withChosenDoc(ctx, env)
}

// ChildDoc is the overlay document a child of ctx's run gets for the unit provider/profile; "" when nothing crosses.
func ChildDoc(ctx context.Context, provider, profile string) string {
	return childDoc(ChildOverlay(ctx, provider, profile))
}

// childDoc is o as its document, "" when it holds nothing but its schema.
func childDoc(o agentconfig.Overlay) string {
	if o.Empty() && !o.Routed() {
		return ""
	}
	return o.Doc()
}

// ChildOverlay is the run's overlay copied for a child (D5), over the env this process reads at the top of a tree.
func ChildOverlay(ctx context.Context, provider, profile string) agentconfig.Overlay {
	own, loadErr := agentconfig.LoadOverlayContext(ctx, "", nil)
	return childOverlayOn(ctx, own, UnitOf(own, provider, profile), loadErr)
}

// childOverlayOn is own copied for a child onto unit, inside own's fence; loadErr is own's read failure.
func childOverlayOn(ctx context.Context, own, unit agentconfig.Overlay, loadErr error) agentconfig.Overlay {
	hop := agentconfig.HopSessionChild
	if inDispatch(ctx) {
		hop = agentconfig.HopDispatchChild
	}
	merged, err := DeriveUnit(ambientOverlay(ctx), own, agentconfig.HopAppend)
	if err == nil {
		merged, err = DeriveUnit(merged, unit, hop)
	}
	if err != nil || loadErr != nil { // fail closed: the unit under a fence that admits no profile
		return agentconfig.Overlay{Provider: unit.Provider, Profile: unit.Profile, Fence: &route.Fence{}}
	}
	return merged
}

// childOverlay is the run's confinement for a child; on that same row the launch's endpoint, identity and account win.
func (p Plan) childOverlay() agentconfig.Overlay {
	ctx := valuesContext(context.Background(), p.Values)
	provider, profile, _ := router.OverlayRoute(ctx)
	own, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	unit := UnitOf(own, provider, profile)
	in, _, _ := route.ReadDoc(proc.Value(ctx, route.OverlayVar)) // only an existing confinement: a bare unit would fence asks
	if u := p.launchUnit(); in.Provider != "" && u.Provider == in.Provider && (u.Profile == in.Profile || in.Profile == route.AutoArm) {
		if u.Profile != in.Profile && strings.Contains(u.Endpoint, "://") {
			u.Endpoint = "" // a row URL never stands for the auto arm
		}
		unit = UnitOf(own, in.Provider, in.Profile)
		unit.Endpoint, unit.Identity, unit.Account = cmp.Or(u.Endpoint, unit.Endpoint), cmp.Or(u.Identity, unit.Identity), cmp.Or(u.Account, unit.Account)
	}
	return childOverlayOn(ctx, own, unit, err)
}

// launchUnit is the launch's unit on the row it ran; a row URL or another provider's scoped field never moves rows.
func (p Plan) launchUnit() route.Route {
	u := p.Unit
	u.Profile = cmp.Or(p.Set[providers.EnvSessionProfile], p.Profile, u.Profile)
	if u.Profile != p.Unit.Profile && strings.Contains(u.Endpoint, "://") {
		u.Endpoint = ""
	}
	if reg, err := providers.LoadShared(); err == nil {
		if row := reg.Row(u.Profile); row != nil && u.Provider != row.Provider {
			if u.Provider != "" {
				u.Endpoint, u.Identity, u.Account = "", "", ""
			}
			u.Provider = row.Provider
		}
	}
	return u
}

// inDispatch reports a run that is a dispatch or a dispatch's child: its id is on ctx or in its run values.
func inDispatch(ctx context.Context) bool {
	return DispatchIDOf(ctx) != "" || proc.Value(ctx, proc.DispatchIDEnv) != ""
}

// DeriveUnit is Derive that also takes a provider-only unit (no profile), which Derive refuses as a document.
func DeriveUnit(base, add agentconfig.Overlay, hop agentconfig.Hop) (agentconfig.Overlay, error) {
	baseOnly, addOnly := splitProviderOnly(&base), splitProviderOnly(&add)
	out, err := base.Derive(add, hop)
	if err != nil {
		return out, err
	}
	kept := cmp.Or(addOnly, baseOnly) // base's unit stands unless add set a full one
	if out.Provider != "" && addOnly == "" {
		kept = ""
	}
	if err := keepsProvider(out.Fence, cmp.Or(baseOnly, base.Provider), cmp.Or(kept, out.Provider)); err != nil || kept == "" {
		return out, err
	}
	out.Endpoint, out.Provider, out.Profile, out.Identity, out.Account = "", kept, "", "", ""
	return out, nil
}

// keepsProvider refuses a provider change under a closed fence.
func keepsProvider(fence *route.Fence, from, to string) error {
	if fence != nil && !fence.Open && from != "" && to != "" && from != to {
		return errors.New("provider: outside the fence (a fenced overlay keeps its provider)")
	}
	return nil
}

// splitProviderOnly clears o's unit when it names a provider and no profile, and returns that provider.
func splitProviderOnly(o *agentconfig.Overlay) string {
	if o.Provider == "" || o.Profile != "" {
		return ""
	}
	p := o.Provider
	o.Provider, o.Identity, o.Account = "", "", ""
	return p
}

// UnitOf is the add that sets own's unit to provider/profile; endpoint, as and account stay only on the same provider.
func UnitOf(own agentconfig.Overlay, provider, profile string) agentconfig.Overlay {
	add := agentconfig.Overlay{Provider: provider, Profile: profile}
	if provider == own.Provider {
		add.Endpoint, add.Identity, add.Account = own.Endpoint, own.Identity, own.Account
	}
	return add
}

// ambientOverlay is this process's own env as overlay keys, read once at the top of a tree (children get it scrubbed).
func ambientOverlay(ctx context.Context) agentconfig.Overlay {
	var o agentconfig.Overlay
	if raw := strings.TrimSpace(proc.Value(ctx, router.EnvFallback)); raw != "" && slices.Contains(route.FallbackModes, router.ParseFallback(raw)) {
		o.Fallback = router.ParseFallback(raw)
	}
	if mode := strings.ToLower(strings.TrimSpace(proc.Value(ctx, rtkwrap.CodemodeEnv))); slices.Contains(route.Codemodes, mode) {
		o.Codemode = mode
	}
	for key, dst := range map[string]**int{maxDispatchChildrenEnv: &o.MaxChildren, maxTreeTokensEnv: &o.MaxTreeTokens, maxDispatchDepthEnv: &o.MaxDepth} {
		if n, err := strconv.Atoi(proc.Value(ctx, key)); err == nil && n >= 0 {
			*dst = &n
		}
	}
	return o
}

// withChosenDoc is a copy of env with the chosen scope proxy in its inline overlay document; a path document is left.
func withChosenDoc(ctx context.Context, env []string) []string {
	i := slices.IndexFunc(env, func(e string) bool { return proc.EnvHasPrefix(e, route.OverlayVar+"=") })
	if i < 0 {
		return env
	}
	_, doc, _ := strings.Cut(env[i], "=")
	o, err := agentconfig.LoadOverlayContext(ctx, doc, nil)
	if err != nil || !strings.HasPrefix(doc, "{") {
		return env
	}
	n := withChosenEndpoint(ctx, o)
	if n.Endpoint == o.Endpoint {
		return env
	}
	out := slices.Clone(env)
	out[i] = route.OverlayVar + "=" + n.Doc()
	return out
}

// withChosenEndpoint sets the chosen team on a whole unit (a daemon choice clears it); direct and URL endpoints stand.
func withChosenEndpoint(ctx context.Context, o agentconfig.Overlay) agentconfig.Overlay {
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	if id == "" {
		return o
	}
	routeMu.Lock()
	label, chosen := chosenByID[id]
	routeMu.Unlock()
	if !chosen || o.Endpoint != "" && o.Endpoint != EndpointProxy && !isTeamLabel(o.Endpoint) {
		return o
	}
	switch {
	case isTeamLabel(label) && o.Provider != "" && o.Profile != "":
		o.Endpoint = label
	case isTeamLabel(o.Endpoint):
		o.Endpoint = ""
	}
	return o
}

// isTeamLabel is a scope proxy endpoint, <team>:<id>; a URL has a colon too.
func isTeamLabel(e string) bool {
	team, id, ok := strings.Cut(e, ":")
	return ok && team != "" && id != "" && !strings.Contains(e, "/")
}

// withRTKQuiet adds the rtk banner and telemetry switches unless env already sets them.
func withRTKQuiet(env []string) []string {
	for _, name := range []string{paths.RTKQuietEnv, paths.RTKTelemetryEnv} {
		if !slices.ContainsFunc(env, func(e string) bool { return proc.EnvHasPrefix(e, name+"=") }) {
			env = append(env, name+"=1")
		}
	}
	return env
}

// vaultLayer merges the vault env under set; set and unset names win, and CODEMODE is never taken from the vault.
func vaultLayer(set map[string]string, unset []string, custom map[string]string) map[string]string {
	merged := maps.Clone(set)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range custom {
		named := func(u string) bool { return proc.EnvNameEqual(u, k) }
		harness := slices.ContainsFunc(slices.Collect(maps.Keys(set)), named)
		if !harness && !slices.ContainsFunc(unset, named) && !proc.EnvNameEqual(k, rtkwrap.CodemodeEnv) {
			merged[k] = v
		}
	}
	return merged
}

// withoutShimPath drops the rtkwrap shim dir from an inherited PATH, so a nested agent's commands stay unshimmed.
func withoutShimPath(env []string) []string {
	shim := rtkwrap.Dir()
	shimInfo, _ := os.Stat(shim)
	return slices.Collect(func(yield func(string) bool) {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && proc.EnvNameEqual(k, "PATH") {
				kv = k + "=" + strings.Join(slices.DeleteFunc(filepath.SplitList(v), func(d string) bool {
					if d == "" {
						return false
					}
					if filepath.Clean(d) == shim {
						return true
					}
					fi, err := os.Stat(d)
					return err == nil && shimInfo != nil && os.SameFile(fi, shimInfo)
				}), string(os.PathListSeparator))
			}
			if !yield(kv) {
				return
			}
		}
	})
}

// withToolchainPath puts the login PATH dirs first when missing, so a non-login launch skips distro gccgo.
func withToolchainPath(env []string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return env
	}
	return slices.Collect(func(yield func(string) bool) {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && proc.EnvNameEqual(k, "PATH") {
				have := filepath.SplitList(v)
				var add []string
				for _, rel := range remote.LoginPathDirs {
					d := filepath.Join(home, filepath.FromSlash(rel))
					if fi, err := os.Stat(d); err == nil && fi.IsDir() && !slices.Contains(have, d) {
						add = append(add, d)
					}
				}
				kv = k + "=" + strings.Join(append(add, have...), string(os.PathListSeparator))
			}
			if !yield(kv) {
				return
			}
		}
	})
}

// withMemoryOrigin hands the child the launching workspace's store origin, so a scratch-folder session uses its memory.
func withMemoryOrigin(env []string) []string {
	if slices.ContainsFunc(env, func(e string) bool { return proc.EnvHasPrefix(e, memory.OriginEnv+"=") }) {
		return env
	}
	cwd, err := os.Getwd()
	if err != nil {
		return env
	}
	if o := memory.LaunchOrigin(cwd); o != "" {
		env = append(env, memory.OriginEnv+"="+o)
	}
	return env
}

// scrub is the Mode's deny scrub; EnvPrescrubbed passes env through.
func (m EnvMode) scrub(ctx context.Context, env, keep []string) (kept, dropped []string) {
	switch m {
	case EnvClaude:
		return proc.Scrub(env, keepSet(ctx, keep))
	case EnvAgent:
		k := keepSet(ctx, keep)
		own := keptLogin(ctx)
		if ls, ok := ctx.Value(loginScrubKey{}).(loginScrub); ok && ls.keepGHToken {
			k["GH_TOKEN"] = true // git and gh in a non-copilot hop need it
		}
		return proc.ScrubAgentCredentials(env, k, own)
	}
	return env, nil
}

type launchAgentKey struct{}

// withLaunchAgent marks name as the agent whose own login env an EnvAgent child keeps ("" = none).
func withLaunchAgent(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, launchAgentKey{}, name)
}

// keptLogin is the agent whose login env an EnvAgent child keeps: a hop's loginScrub, else the spec's Agent.
func keptLogin(ctx context.Context) string {
	if ls, ok := ctx.Value(loginScrubKey{}).(loginScrub); ok {
		return ls.own
	}
	agentName, _ := ctx.Value(launchAgentKey{}).(string)
	return agentName
}

type unsetKey struct{}

type dispatchIDKey struct{}
type dispatchDepthKey struct{}

// withDispatchID carries the dispatch record id every child env built under ctx is marked with.
func withDispatchID(ctx context.Context, id string, depth int) context.Context {
	ctx = context.WithValue(ctx, dispatchDepthKey{}, depth)
	return context.WithValue(ctx, dispatchIDKey{}, id)
}

// withUnsetEnv carries names every child env built under ctx drops from its inherited base (an overlay's unset).
func withUnsetEnv(ctx context.Context, names []string) context.Context {
	if len(names) == 0 {
		return ctx
	}
	return context.WithValue(ctx, unsetKey{}, slices.Clone(names))
}

// dropper reports a base entry the child must not inherit: unset by the spec or ctx, or assigned again after it.
func (s EnvSpec) dropper(ctx context.Context) func(string) bool {
	drop := map[string]bool{}
	names, _ := ctx.Value(unsetKey{}).([]string)
	for _, n := range slices.Concat(s.Unset, names) {
		drop[proc.EnvFold(n)] = true
	}
	for _, kv := range slices.Concat(s.Set, s.Extra) {
		n, _, _ := strings.Cut(kv, "=")
		drop[proc.EnvFold(n)] = true
	}
	if s.PWD != "" {
		drop[proc.EnvFold("PWD")] = true
	}
	return func(kv string) bool { n, _, _ := strings.Cut(kv, "="); return drop[proc.EnvFold(n)] }
}

// scrubIsolated drops other profiles' credentials from the INHERITED env under batch isolation; launch values follow.
func scrubIsolated(ctx context.Context, inherited, keepEnv []string) []string {
	inherited = dropForeignLogins(ctx, inherited, keepEnv)
	if p := ownProviderFrom(ctx); p != nil {
		return scrubExcept(ctx, inherited, p.Name, append(providers.CredentialNames(p), keepEnv...), keepEnv)
	}
	if ctx.Value(isolateKey{}) == nil {
		return inherited
	}
	ls, _ := ctx.Value(loginScrubKey{}).(loginScrub)
	return scrubExcept(ctx, inherited, "", append(slices.Clone(proc.OwnLoginEnv[ls.own]), keepEnv...), keepEnv)
}

// dropForeignLogins drops the own-login env ctx's loginScrub forbids (a coworker hop, a probe); else env as is.
func dropForeignLogins(ctx context.Context, env, keepEnv []string) []string {
	ls, ok := ctx.Value(loginScrubKey{}).(loginScrub)
	if !ok {
		return env
	}
	return proc.Drop(env, keepSet(ctx, keepEnv), proc.ForeignLogins(ls.own, ls.keepGHToken))
}

type loginScrubKey struct{}

// loginScrub drops every agent's own-login env but own's ("" = none); keepGHToken spares GH_TOKEN for git and gh.
type loginScrub struct {
	own         string
	keepGHToken bool
}

// ProbeContext is ctx for a vendor CLI probe: no registry credential, and no own-login env but login's ("" = none).
func ProbeContext(ctx context.Context, login string) context.Context {
	ctx = context.WithValue(withIsolation(ctx), ownProviderKey{}, (*providers.Provider)(nil))
	return context.WithValue(ctx, loginScrubKey{}, loginScrub{own: login})
}

// scrubExcept drops every registry credential name of the profiles other than ownName, except those in mine.
func scrubExcept(ctx context.Context, env []string, ownName string, mine, keepEnv []string) []string {
	reg, err := providers.Load()
	if err != nil { // fail closed: foreign owners are unknown, so drop every credential-shaped name not held
		return proc.Drop(env, keepSet(ctx, mine), proc.ScrubDenyCredentials)
	}
	foreign := map[string]bool{}
	for _, p := range reg.Providers {
		if p == nil || p.Name == ownName {
			continue
		}
		for _, n := range providers.CredentialNames(p) {
			if !slices.ContainsFunc(mine, func(m string) bool { return proc.EnvNameEqual(m, n) }) {
				foreign[n] = true
			}
		}
	}
	keep := keepSet(ctx, keepEnv)
	return proc.Drop(env, keep, slices.Collect(maps.Keys(foreign)))
}

// envNames are the names of KEY=VALUE entries.
func envNames(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}
