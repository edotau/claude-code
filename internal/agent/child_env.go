// child_env.go — the one env builder for every headless agent child: ChildCmd, claude's launch, the Copilot SDK.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/shapes"
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
	if kv, ok := ctx.Value(permissionsKey{}).(string); ok { // the dispatch's policy wins over any inherited copy
		env = append(slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, DispatchPermissionsEnv+"=") }), kv)
	}
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return withMemoryOrigin(env)
}

// withMemoryOrigin hands the child the launching workspace's store origin, so a scratch-folder session uses its memory.
func withMemoryOrigin(env []string) []string {
	if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, memstore.OriginEnv+"=") }) {
		return env
	}
	cwd, err := os.Getwd()
	if err != nil {
		return env
	}
	if o := memstore.LaunchOrigin(cwd); o != "" {
		env = append(env, memstore.OriginEnv+"="+o)
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

// DispatchPermissionsEnv carries a dispatch's Permissions as JSON to every agent child, for the in-agent permission-gate.
const DispatchPermissionsEnv = "HARNESS_DISPATCH_PERMISSIONS"

type permissionsKey struct{}

// inheritPermissions keeps an outer dispatch's Deny rules and plan mode in a nested dispatch, so an inner policy
// can never drop them; with no inner policy the outer one applies whole.
func inheritPermissions(p Permissions) (Permissions, error) {
	raw := os.Getenv(DispatchPermissionsEnv)
	if raw == "" {
		return p, nil
	}
	var outer Permissions
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		return p, fmt.Errorf("unreadable %s from the outer dispatch: %w", DispatchPermissionsEnv, err)
	}
	if p.IsZero() {
		return outer, nil
	}
	p.Deny = append(slices.Clone(p.Deny), outer.Deny...)
	if outer.Mode == "plan" {
		p.Mode = "plan"
	}
	return p, nil
}

// withPermissionsEnv makes every child env built under ctx carry p as DispatchPermissionsEnv; a zero p changes nothing.
func withPermissionsEnv(ctx context.Context, p Permissions) context.Context {
	if p.IsZero() {
		return ctx
	}
	raw, _ := json.Marshal(p) // strings and string slices always marshal
	return context.WithValue(ctx, permissionsKey{}, DispatchPermissionsEnv+"="+string(raw))
}

type unsetKey struct{}

// WithUnsetEnv carries names every child env built under ctx drops from its inherited base (an overlay's unset).
func WithUnsetEnv(ctx context.Context, names []string) context.Context {
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

// credentialPatterns are the credential-shaped names dropped when the registry cannot say whose they are.
var credentialPatterns = []string{"*_KEY", "*_TOKEN", "*_SECRET", "*_PASSWORD", "*_CREDENTIALS"}

// scrubExcept drops every registry credential name of the profiles other than ownName, except those in mine.
func scrubExcept(env []string, ownName string, mine, keepEnv []string) []string {
	reg, err := providers.Load()
	if err != nil { // fail closed: foreign owners are unknown, so drop every credential-shaped name not held
		return proc.Drop(env, proc.EnvKeepSet(mine), append(slices.Clone(proc.ScrubDeny), credentialPatterns...))
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

// EnvNames are the names of KEY=VALUE entries.
func EnvNames(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		out[i], _, _ = strings.Cut(kv, "=")
	}
	return out
}

// ChildCmd builds an exec.Cmd in its own process group with inherited credentials scrubbed; agent also scrubs the vault key.
func ChildCmd(ctx context.Context, bin string, argv, env []string, dir, stdin string, keepEnv []string, cli bool) (*exec.Cmd, *retry.TailBuffer) {
	cmd := exec.CommandContext(ctx, bin, argv...)
	proc.OwnGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	mode := EnvClaude
	if cli {
		mode = EnvAgent
	}
	cmd.Env = EnvSpec{Mode: mode, Keep: keepEnv, Set: env, PWD: dir}.Environ(ctx) // launch values after the scrub
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	tail := &retry.TailBuffer{}
	cmd.Stderr = io.Writer(tail) // the dispatch log, when ctx carries one, sees stderr as it arrives
	if sk := wire.StderrLogFrom(ctx); sk != nil {
		cmd.Stderr = io.MultiWriter(tail, sk.Stream())
	}
	return cmd, tail
}

// childErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err for classification.
func childErr(ctx context.Context, name string, err error, tail *retry.TailBuffer) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if t := strings.TrimSpace(tail.String()); t != "" {
		if len(t) > 600 {
			t = "…" + strings.ToValidUTF8(t[len(t)-600:], "")
		}
		return fmt.Errorf("%s: %w: %s", name, err, shapes.Redact(t))
	}
	return fmt.Errorf("%s: %w", name, err)
}
