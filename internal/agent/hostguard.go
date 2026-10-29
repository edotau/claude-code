package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/envscrub"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

type (
	ownProviderKey struct{}
	isolateKey     struct{}
)

// withIsolation makes every hop under ctx drop other profiles' credential env (batch tasks run concurrently).
func withIsolation(ctx context.Context) context.Context {
	return context.WithValue(ctx, isolateKey{}, true)
}

// withOwnProvider marks p as the one profile this hop may hold credentials for, when ctx asks for isolation.
func withOwnProvider(ctx context.Context, p *providers.Provider) context.Context {
	if ctx.Value(isolateKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, ownProviderKey{}, p)
}

func ownProviderFrom(ctx context.Context) *providers.Provider {
	p, _ := ctx.Value(ownProviderKey{}).(*providers.Provider)
	return p
}

type frozenKey struct{}

// frozenTarget is the copilot target a leg resolved once, before its permit: the gate may move it off the GitHub login.
type frozenTarget struct {
	t    providers.Target
	own  bool
	note string
}

// freezeCopilot resolves copilot's real target once, so the permit, cooldown key, guard and runner all use the same
// account. When the gate moved the leg to another provider, the leg bills that provider's account instead.
func freezeCopilot(ctx context.Context, leg Leg, req Request) (context.Context, Leg) {
	if leg.Agent.Name() != "copilot" {
		return ctx, leg
	}
	t, own, note, err := vendorTargetNote(req, "copilot")
	if err != nil {
		return ctx, leg // the runner reports its own resolution failure
	}
	if !own && t.Provider != nil && t.Provider.Auth.Login != "copilot" {
		leg.Account, leg.Billing = profileAccount(t.Provider), billingOf(t.Provider)
	}
	return context.WithValue(ctx, frozenKey{}, frozenTarget{t, own, note}), leg
}

// vendorTargetCtx is vendorTarget, except a target frozen on ctx is used as is.
func vendorTargetCtx(ctx context.Context, req Request, name string) (providers.Target, bool, error) {
	if f, ok := ctx.Value(frozenKey{}).(frozenTarget); ok && name == "copilot" {
		if f.note != "" {
			fmt.Fprintln(os.Stderr, f.note)
		}
		t := f.t // the provider and account are frozen; the model follows this attempt's request (a quota downgrade)
		if m := t.Provider; m != nil && slices.Contains(models.Slots, req.Model) && m.Models[req.Model] != "" {
			t.Model = m.Models[req.Model]
		}
		return t, f.own, nil
	}
	return vendorTarget(req, name)
}

// guardRouteHost fails a hop before any child starts when p's credential was issued for another host than its route.
func guardRouteHost(ctx context.Context, p *providers.Provider) error {
	if p.Preset == "" {
		return nil
	}
	cred, err := providers.Credential(ctx, p)
	if errors.Is(err, providers.ErrPassthrough) || (err == nil && cred == "") {
		return nil // no minted credential to misroute
	}
	if err != nil {
		return err // an unresolvable credential must not skip the host check
	}
	return providers.CheckRouteHost(p, cred)
}

// guardProvider is the provider whose credential the hop sends; nil for an unpinned vendor leg (own login).
func guardProvider(r Runner, req Request) (*providers.Provider, error) {
	if l, ok := r.(hubLeg); ok {
		return l.provider, nil
	}
	if _, vendor := vendors.Lookup(r.Name()); vendor && r.Name() != "claude" {
		// the same target the runner picks: --provider, a provider:model prefix, the session, the copilot gate
		t, own, _, err := vendorTargetNote(req, r.Name())
		if err != nil {
			if req.Provider != "" {
				return nil, err
			}
			return nil, nil // the runner reports its own resolution failure with its own kind
		}
		if own {
			return nil, nil
		}
		return t.Provider, nil
	}
	if req.Provider == "" && r.Name() != "claude" {
		return nil, nil
	}
	_, t, err := resolveTarget(req)
	if err != nil {
		return nil, err
	}
	return t.Provider, nil
}

// guardAttempt runs inside a timed attempt: it validates the hop's credential and marks its one own profile on ctx.
func guardAttempt(ctx context.Context, r Runner, req Request) (context.Context, *Error) {
	var gp *providers.Provider
	var err error
	if f, ok := ctx.Value(frozenKey{}).(frozenTarget); ok && r.Name() == "copilot" {
		if !f.own {
			gp = f.t.Provider
		}
	} else {
		gp, err = guardProvider(r, req)
	}
	if err != nil {
		return ctx, Classify(r.Name(), err, false)
	}
	if gp == nil {
		return ctx, nil
	}
	if err := checkIsolation(ctx); err != nil {
		return ctx, &Error{Kind: KindFatal, Agent: r.Name(), Err: err}
	}
	if _, hub := r.(hubLeg); hub { // a hub leg sends the opencode login's credential, not the registry's
		return withOwnProvider(ctx, gp), nil
	}
	if err := guardRouteHost(ctx, gp); err != nil {
		if _, ok := errors.AsType[*providers.HostMismatchError](err); ok {
			return ctx, &Error{Kind: KindAuth, Agent: r.Name(), Err: err, AuthReason: fallbackReason(err)} // misconfigured: move on
		}
		return ctx, Classify(r.Name(), err, false) // a transient auth.command/OAuth failure stays retryable
	}
	return withOwnProvider(ctx, gp), nil
}

// checkIsolation fails an isolated hop up front when the registry cannot load, since the scrub would then be skipped.
func checkIsolation(ctx context.Context) error {
	if ctx.Value(isolateKey{}) == nil {
		return nil
	}
	_, err := providers.Load()
	return err
}

// blankForeignCredentials adds NAME= for each foreign credential in the process env that env (the launch's own
// values) does not set, for a child (the Copilot SDK) that merges os.Environ itself and applies env last.
func blankForeignCredentials(ctx context.Context, env, keepEnv []string) []string {
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
	if own == nil {
		return env
	}
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
	keep := envscrub.KeepSet(keepEnv)
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return foreign[name] && !keep[name]
	})
}
