// env.go — the one env builder for every headless agent child: ChildCmd, claude's launch, the Copilot SDK.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/rtkwrap"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
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
	base = withoutShimPath(base) // only Pi's own launch (Set) puts the rtk shim dir on PATH
	drop := s.dropper(ctx)
	env := slices.DeleteFunc(slices.Clone(scrubIsolated(ctx, base, s.Keep)), drop)
	if doc := confinementDocContext(ctx); doc != "" && drop(route.OverlayVar+"=") { // one hop: only the confinement goes down
		env = append(env, route.OverlayVar+"="+doc)
	}
	// a coworker hop or probe drops a launch-set foreign login too (opencode's LaunchEnv adds OPENCODE_API_KEY everywhere)
	env = append(append(env, dropForeignLogins(ctx, s.Set, s.Keep)...), s.Extra...)
	if kv, ok := ctx.Value(permissionsKey{}).(string); ok { // the dispatch's policy wins over any inherited copy
		env = append(slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, DispatchPermissionsEnv+"=") }), kv)
	}
	if id, ok := ctx.Value(dispatchIDKey{}).(string); ok { // marks the child so `agents cancel --lost` can find it once orphaned
		depth, _ := ctx.Value(dispatchDepthKey{}).(int)
		env = append(slices.DeleteFunc(env, func(e string) bool {
			return strings.HasPrefix(e, proc.DispatchIDEnv+"=") || strings.HasPrefix(e, proc.DispatchDepthEnv+"=")
		}), proc.DispatchIDEnv+"="+id, proc.DispatchDepthEnv+"="+strconv.Itoa(depth))
	}
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return withMemoryOrigin(env)
}

// withoutShimPath drops the rtkwrap shim dir from an inherited PATH, so a nested agent's commands stay unshimmed.
func withoutShimPath(env []string) []string {
	shim := rtkwrap.Dir()
	shimInfo, _ := os.Stat(shim)
	return slices.Collect(func(yield func(string) bool) {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				kv = "PATH=" + strings.Join(slices.DeleteFunc(filepath.SplitList(v), func(d string) bool {
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

// withMemoryOrigin hands the child the launching workspace's store origin, so a scratch-folder session uses its memory.
func withMemoryOrigin(env []string) []string {
	if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, memory.OriginEnv+"=") }) {
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

// DispatchPermissionsEnv carries a dispatch's Permissions as JSON to every agent child, for the permission-gate.
const DispatchPermissionsEnv = "HARNESS_DISPATCH_PERMISSIONS"

type permissionsKey struct{}

// InheritPermissions is p under the outer dispatch's policy in ctx's values: a nested run can only narrow it.
func InheritPermissions(ctx context.Context, p permission.Permissions) (permission.Permissions, error) {
	raw := proc.Value(ctx, DispatchPermissionsEnv)
	if raw == "" {
		return p, nil
	}
	var outer permission.Permissions
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		return p, fmt.Errorf("unreadable %s from the outer dispatch: %w", DispatchPermissionsEnv, err)
	}
	return layerOuter(p, outer), nil
}

// layerOuter is p under an outer dispatch's policy: outer whole when p is zero, else p plus outer's deny, ask and plan.
func layerOuter(p, outer permission.Permissions) permission.Permissions {
	if p.IsZero() {
		return outer
	}
	p.Deny, p.Ask = permission.Union(p.Deny, outer.Deny), permission.Union(p.Ask, outer.Ask)
	if outer.Mode == permission.PermPlan {
		p.Mode = permission.PermPlan
	}
	return p
}

// WithPermissionsEnv exports p to children and snapshots the policy on the dispatch record, including an empty policy.
func WithPermissionsEnv(ctx context.Context, p permission.Permissions) context.Context {
	if id := DispatchIDOf(ctx); id != "" {
		_, _, err := updateRecord(id, func(r Record) (Record, bool) {
			r.ExportedPolicy = &p
			return r, true
		})
		if err != nil {
			fmt.Fprintf(NoticeOut, "claude-code: dispatch %s exported policy not saved: %v\n", id, err)
		}
	}
	if p.IsZero() {
		return ctx
	}
	raw, _ := json.Marshal(p) // strings and string slices always marshal
	return context.WithValue(ctx, permissionsKey{}, DispatchPermissionsEnv+"="+string(raw))
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
		drop[n] = true
	}
	for _, kv := range slices.Concat(s.Set, s.Extra) {
		n, _, _ := strings.Cut(kv, "=")
		drop[n] = true
	}
	if s.PWD != "" {
		drop["PWD"] = true
	}
	return func(kv string) bool { n, _, _ := strings.Cut(kv, "="); return drop[n] }
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
			if !slices.Contains(mine, n) {
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
