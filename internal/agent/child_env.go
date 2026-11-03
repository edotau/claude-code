// child_env.go — the one env builder for every headless agent child: ChildCmd, claude's launch, the Copilot SDK.
package agent

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"

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
}

// Environ builds the child env; ctx carries batch isolation and the hop's own provider (hostguard.go).
func (s EnvSpec) Environ(ctx context.Context) []string {
	base := s.Base
	if base == nil && s.Mode != EnvPrescrubbed {
		base = os.Environ()
	}
	base, _ = s.Mode.scrub(base, s.Keep)
	env := slices.DeleteFunc(slices.Clone(scrubIsolated(ctx, base, s.Keep)), s.dropper(ctx))
	env = append(append(env, s.Set...), s.Extra...)
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return env
}

// scrub is the Mode's deny scrub; EnvPrescrubbed passes env through.
func (m EnvMode) scrub(env, keep []string) (kept, dropped []string) {
	switch m {
	case EnvClaude:
		return proc.Scrub(env, proc.EnvKeepSet(keep))
	case EnvAgent:
		return proc.ScrubAgent(env, proc.EnvKeepSet(keep))
	}
	return env, nil
}

type unsetKey struct{}

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

// scrubIsolated drops other profiles' credentials from the INHERITED env under batch isolation; an own-login hop (no
// profile) keeps no profile's credentials. Launch-owned values are appended after this, so they are never scrubbed.
func scrubIsolated(ctx context.Context, inherited, keepEnv []string) []string {
	if p := ownProviderFrom(ctx); p != nil {
		return scrubForeignCredentials(inherited, p, keepEnv)
	}
	if ctx.Value(isolateKey{}) == nil {
		return inherited
	}
	return scrubExcept(inherited, "", keepEnv, keepEnv)
}

// scrubForeignCredentials drops every other profile's credential env name from env, so a hop holds only its own.
func scrubForeignCredentials(env []string, own *providers.Provider, keepEnv []string) []string {
	return scrubExcept(env, own.Name, append(providers.CredentialNames(own), keepEnv...), keepEnv)
}

// scrubExcept drops every registry credential name of the profiles other than ownName, except those in mine.
func scrubExcept(env []string, ownName string, mine, keepEnv []string) []string {
	reg, err := providers.Load()
	if err != nil {
		return env
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
	keep := proc.EnvKeepSet(keepEnv)
	return proc.Drop(env, keep, slices.Collect(maps.Keys(foreign)))
}
