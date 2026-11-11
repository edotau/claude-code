package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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

// frozenTarget is the copilot target resolved once, before the permit; the gate may move it off the GitHub login.
type frozenTarget struct {
	t    providers.Target
	own  bool
	note string
	once *sync.Once // the gate note prints once per dispatch, not on every retry
}

// freezeCopilot resolves copilot's real target once, so the permit, cooldown key, guard and agent all use the same
// account. When the gate moved the dispatch to another provider, the dispatch bills that provider's account instead.
func freezeCopilot(ctx context.Context, disp Dispatch, req RunOptions) (context.Context, Dispatch) {
	if disp.Agent.Name() != "copilot" {
		return ctx, disp
	}
	t, own, note, err := SchemaFor("copilot").targetNote(req)
	if err != nil {
		return ctx, disp // the agent reports its own resolution failure
	}
	if !own && t.Provider != nil && t.Provider.Auth.Login != "copilot" {
		disp = withBilling(disp, t.Provider)
	}
	return context.WithValue(ctx, frozenKey{}, frozenTarget{t, own, note, &sync.Once{}}), disp
}

// ownLoginActual re-keys a dispatch put on the agent's own login when the agent really resolves another profile.
func ownLoginActual(disp Dispatch, req RunOptions) Dispatch {
	name := disp.Agent.Name()
	if name == "copilot" || name == "claude" || name == "opencode" || !ownLoginAccount(name, disp.Account) {
		return disp
	}
	t, own, _, err := SchemaFor(name).targetNote(req)
	if err != nil || own || t.Provider == nil || t.Provider.Auth.Login == name {
		return disp
	}
	return withBilling(disp, t.Provider)
}

// TargetCtx is target, except a target frozen on ctx is used as is.
func (s Schema) TargetCtx(ctx context.Context, req RunOptions) (providers.Target, bool, error) {
	if f, ok := ctx.Value(frozenKey{}).(frozenTarget); ok && s.Name == "copilot" {
		if f.note != "" {
			f.once.Do(func() { fmt.Fprintln(NoticeOut, f.note) })
		}
		t := f.t // the provider and account are frozen; the model follows this attempt's request (a quota downgrade)
		if m := t.Provider; m != nil && slices.Contains(models.AliasNames, req.Model) && m.Models[req.Model] != "" {
			t.Model = m.Models[req.Model]
		}
		return t, f.own, nil
	}
	return s.Target(req)
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

// guardProvider is the provider whose credential the hop sends; nil for an unpinned agent dispatch (own login).
func guardProvider(r Engine, req RunOptions) (*providers.Provider, error) {
	if l, ok := r.(HubEngine); ok {
		return l.HubProvider(), nil
	}
	s, known := LookupSchema(r.Name())
	if known && s.External() {
		// the same target the agent picks: --provider, a provider:model prefix, the session, the copilot gate
		t, own, _, err := s.targetNote(req)
		if err != nil {
			if req.Provider != "" {
				return nil, err
			}
			return nil, nil // the agent reports its own resolution failure with its own kind
		}
		if own {
			return nil, nil
		}
		return t.Provider, nil
	}
	if req.Provider == "" && !known {
		return nil, nil
	}
	_, t, err := ResolveRequestTarget(req)
	if err != nil {
		return nil, err
	}
	return t.Provider, nil
}

// hopProvider is guardProvider, except a frozen copilot target is used as is (nil on its own login).
func hopProvider(ctx context.Context, r Engine, req RunOptions) (*providers.Provider, error) {
	f, ok := ctx.Value(frozenKey{}).(frozenTarget)
	if !ok || r.Name() != "copilot" {
		return guardProvider(r, req)
	}
	if f.own {
		return nil, nil
	}
	return f.t.Provider, nil
}

// guardAttempt runs inside a timed attempt: it validates the hop's credential and marks its one own profile on ctx.
func guardAttempt(ctx context.Context, r Engine, req RunOptions) (context.Context, *retry.Error) {
	arm := router.ArmProfile()
	if arm != "" { // inside databricks:<p> every hop keeps only its own credential names
		ctx = withIsolation(ctx)
	}
	gp, err := hopProvider(ctx, r, req)
	if err != nil {
		return ctx, retry.Classify(r.Name(), err, false)
	}
	// before the own-login return: that hop is scrubbed and route-checked too
	if err := cmp.Or(checkIsolation(ctx), guardArm(arm, r, gp)); err != nil {
		return ctx, &retry.Error{Kind: retry.KindFatal, Agent: r.Name(), Err: err}
	}
	if arm != "" {
		ctx = withRouteLogins(ctx, arm, r.Name(), gp)
	}
	if gp == nil {
		return ctx, nil
	}
	if _, isHub := r.(HubEngine); isHub { // a hub dispatch sends the opencode login's credential, not the registry's
		return withOwnProvider(ctx, gp), nil
	}
	if err := guardRouteHost(ctx, gp); err != nil {
		if _, ok := errors.AsType[*providers.HostMismatchError](err); ok {
			return ctx, &retry.Error{Kind: retry.KindAuth, Agent: r.Name(), Err: err, AuthReason: FallbackReason(err)} // misconfigured: move on
		}
		return ctx, retry.Classify(r.Name(), err, false) // a transient auth.command/OAuth failure stays retryable
	}
	return ownCredentials(ctx, arm, r, gp), nil
}

// ownCredentials marks gp the hop's own profile; in the route a proxied hop (claude's too) inherits no workspace token.
func ownCredentials(ctx context.Context, arm string, r Engine, gp *providers.Provider) context.Context {
	if arm != "" && (r.Name() != "claude" || gp.Name == arm) {
		return ctx
	}
	return withOwnProvider(ctx, gp)
}

// withRouteLogins: a hop on the armed workspace keeps no agent's own-login env, a first-party hop only its agent's.
func withRouteLogins(ctx context.Context, arm, agentName string, gp *providers.Provider) context.Context {
	own := agentName
	if gp != nil && gp.Name == arm {
		own = ""
	}
	return context.WithValue(ctx, loginScrubKey{}, loginScrub{own: own, keepGHToken: agentName != "copilot"})
}

// guardArm refuses a hop outside databricks:<arm>: only that workspace (not AI Gateway) or the agent's own login runs.
func guardArm(arm string, r Engine, gp *providers.Provider) error {
	if arm == "" || gp == nil { // gp nil: the agent's own sign-in, scrubbed of every registry credential
		return nil
	}
	switch {
	case gp.IsAIGateway():
		return fmt.Errorf("%s: profile %s is on the AI Gateway wire, outside databricks:%s", r.Name(), gp.Name, arm)
	case gp.Name == arm && gp.Preset == providers.PresetDatabricks, armOwnLogin(r.Name(), gp):
		return nil
	}
	return fmt.Errorf("%s: profile %s is outside databricks:%s", r.Name(), gp.Name, arm)
}

// armOwnLogin: gp is agentName's own sign-in, claude's Anthropic subscription or opencode's Go plan.
func armOwnLogin(agentName string, gp *providers.Provider) bool {
	if agentName == "claude" {
		return gp.Provider == "anthropic" && gp.Auth.Method() == providers.AuthCLI
	}
	if agentName == "opencode" && gp.Name == router.ArmOpencodeSub { // the hub's synthetic Go plan: no registry row
		return true
	}
	// an unreadable registry reads as no own login: the hop is refused, never run unchecked
	return ownLogin(func() *providers.Registry { reg, _ := providers.Load(); return reg }, agentName, gp.Name)
}

// checkIsolation fails an isolated hop up front when the registry cannot load, since the scrub would then be skipped.
func checkIsolation(ctx context.Context) error {
	if ctx.Value(isolateKey{}) == nil {
		return nil
	}
	_, err := providers.Load()
	return err
}

// withBilling re-keys disp to p's account and billing.
func withBilling(disp Dispatch, p *providers.Provider) Dispatch {
	disp.Account, disp.Billing = p.Account(), router.DefaultBilling(p)
	return disp
}
