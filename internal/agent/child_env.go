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
	switch s.Mode {
	case EnvClaude:
		base, _ = proc.Scrub(base, proc.EnvKeepSet(s.Keep))
	case EnvAgent:
		base, _ = proc.ScrubAgent(base, proc.EnvKeepSet(s.Keep))
	}
	env := append(append(scrubIsolated(ctx, base, s.Keep), s.Set...), s.Extra...)
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return env
}

// BlankForeignCredentials adds NAME= for each foreign credential in the process env that env (the launch's own
// values) does not set, for a child (the Copilot SDK) that merges os.Environ itself and applies env last.
func BlankForeignCredentials(ctx context.Context, env, keepEnv []string) []string {
	kept := map[string]bool{}
	for _, kv := range scrubIsolated(ctx, os.Environ(), keepEnv) {
		kept[kv] = true
	}
	owned := envNames(env)
	out := slices.Clone(env)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !kept[kv] && !slices.Contains(owned, name) {
			out = append(out, name+"=")
		}
	}
	return out
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
