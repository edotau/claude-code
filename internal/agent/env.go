// env.go — the one env builder for every headless agent child: ChildCmd, claude's launch, the Copilot SDK.
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

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
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
	// a coworker hop or probe drops a launch-set foreign login too (opencode's LaunchEnv adds OPENCODE_API_KEY everywhere)
	env = append(append(env, dropForeignLogins(ctx, s.Set, s.Keep)...), s.Extra...)
	if kv, ok := ctx.Value(permissionsKey{}).(string); ok { // the dispatch's policy wins over any inherited copy
		env = append(slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, DispatchPermissionsEnv+"=") }), kv)
	}
	if id, ok := ctx.Value(dispatchIDKey{}).(string); ok { // marks the child so `agents reap` can find it once orphaned
		env = append(slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, proc.DispatchIDEnv+"=") }),
			proc.DispatchIDEnv+"="+id)
	}
	if s.PWD != "" {
		env = append(env, "PWD="+s.PWD)
	}
	return withMemoryOrigin(env)
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

type dispatchIDKey struct{}

// withDispatchID carries the dispatch record id every child env built under ctx is marked with.
func withDispatchID(ctx context.Context, id string) context.Context {
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

// scrubIsolated drops other profiles' credentials from the INHERITED env under batch isolation; an own-login hop (no
// profile) keeps no profile's credentials. Launch-owned values come after this; only foreign logins leave them.
func scrubIsolated(ctx context.Context, inherited, keepEnv []string) []string {
	inherited = dropForeignLogins(ctx, inherited, keepEnv)
	if p := ownProviderFrom(ctx); p != nil {
		return scrubExcept(inherited, p.Name, append(providers.CredentialNames(p), keepEnv...), keepEnv)
	}
	if ctx.Value(isolateKey{}) == nil {
		return inherited
	}
	ls, _ := ctx.Value(loginScrubKey{}).(loginScrub)
	return scrubExcept(inherited, "", append(slices.Clone(proc.OwnLoginEnv[ls.own]), keepEnv...), keepEnv)
}

// dropForeignLogins drops the own-login env ctx's loginScrub forbids (a coworker hop, a probe); else env as is.
func dropForeignLogins(ctx context.Context, env, keepEnv []string) []string {
	ls, ok := ctx.Value(loginScrubKey{}).(loginScrub)
	if !ok {
		return env
	}
	return proc.Drop(env, proc.EnvKeepSet(keepEnv), proc.ForeignLogins(ls.own, ls.keepGHToken))
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

// envNames are the names of KEY=VALUE entries.
func envNames(env []string) []string {
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

// ChildErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err for classification.
func ChildErr(ctx context.Context, name string, err error, tail *retry.TailBuffer) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", name, ctx.Err())
	}
	if t := stderrTail(tail.String()); t != "" {
		return fmt.Errorf("%s: %w: %s", name, err, t)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// stderrTailBytes caps the stderr an error quotes.
const stderrTailBytes = 600

// stderrTail is a child's stderr fit for an error: trimmed, its last stderrTailBytes, secrets redacted.
func stderrTail(s string) string {
	t := strings.TrimSpace(s)
	if len(t) > stderrTailBytes {
		t = "…" + strings.ToValidUTF8(t[len(t)-stderrTailBytes:], "") // the cut may split a rune
	}
	return textutil.RedactSecrets(t)
}
