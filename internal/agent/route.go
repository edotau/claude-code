package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vendors"
)

// Leg is one routed step: the runner plus the cell it runs (model, effort) and the account it bills.
type Leg struct {
	Runner                  Runner
	Model, Effort           string
	Account, Billing        string
	keepCaller, unroutedLeg bool // keepCaller: the wanted leg keeps the caller's own --model
}

// Chain is a routed dispatch: Want is the runner asked for, Legs run in order, Skipped are gate drops.
type Chain struct {
	Want    string
	Legs    []Leg
	Skipped []string
}

// Single is a one-leg chain on r with the caller's request untouched (an explicit backend, or a test).
func Single(r Runner) Chain {
	return Chain{Want: r.Name(), Legs: []Leg{{Runner: r, keepCaller: true, unroutedLeg: true}}}
}

// State seams: tests point them away from the real quota.json.
var (
	loadState     = route.LoadState
	markExhausted = route.MarkExhausted
	markAuth      = route.MarkAuth
)

// RouteChain turns a pick at tier into legs; the wanted runner's leg keeps an explicit caller model, every other leg
// runs its own cell at the same tier. native keeps a wanted opencode leg on opencode's own config instead of the hub.
func RouteChain(ctx context.Context, want, tier, mode string, explicitModel, native bool, o route.Opts) (Chain, route.Result, error) {
	tb, err := route.Load()
	if err != nil {
		return Chain{}, route.Result{}, err
	}
	o.Fallback = mode
	if o.Account == nil {
		o.Account = resolveAccount
	}
	// --native opencode runs on opencode's own config first: no hub login gate, no table cell
	pickWant, nativeLead := want, native && want == "opencode"
	if nativeLead {
		pickWant = ""
	}
	pick := tb.Pick(ctx, tier, pickWant, loadState(), o)
	reg, _ := providers.Load()
	c := Chain{Want: want}
	if nativeLead {
		c.Legs = append(c.Legs, Leg{Runner: vendorRunner{"opencode"}, keepCaller: true, unroutedLeg: true})
		if mode == FallbackNone {
			return c, pick, nil
		}
	}
	for _, s := range pick.Skipped {
		if !nativeLead || s.Runner != "opencode" {
			c.Skipped = append(c.Skipped, s.Runner+": "+s.Reason)
		}
	}
	for _, cand := range pick.Candidates {
		if nativeLead && cand.Runner == "opencode" {
			continue
		}
		leg := Leg{Model: cand.Model, Effort: cand.Effort, Account: cand.Account, Billing: cand.Billing}
		leg.keepCaller = cand.Runner == want && explicitModel
		switch {
		case cand.Runner == "opencode" && (!native || cand.Runner != want):
			r, model, err := hubLegFor(reg, cand.Model)
			if err != nil {
				c.Skipped = append(c.Skipped, "opencode: "+err.Error())
				continue
			}
			leg.Runner, leg.Model = watchFreeLeg(r, cand.Billing), model
		default:
			r, err := Lookup(cand.Runner)
			if err != nil {
				return Chain{}, pick, err
			}
			leg.Runner = r
		}
		c.Legs = append(c.Legs, leg)
	}
	return c, pick, nil
}

// watchFreeLeg sets the stall watchdog on r when billing is free; any other runner (or billing) passes through.
func watchFreeLeg(r Runner, billing string) Runner {
	if billing != route.BillFree {
		return r
	}
	if hl, ok := r.(hubLeg); ok {
		hl.firstOutput = firstOutputDeadline()
		return hl
	}
	return r
}

// resolveAccount is the account a runner's leg actually bills: "" keeps the table's key (the runner's own login or
// subscription); a vendor resolving to another provider (an API key, a BYOK route) bills "<runner>:<provider>".
func resolveAccount(runner string, c route.Cell) (string, string) {
	var t providers.Target
	switch runner {
	case "opencode":
		return "", "" // the cell's provider prefix is the account; route.Configured gates its credential
	case "claude":
		_, ct, err := resolveTarget(Request{Model: c.Model})
		if err != nil || ct.Provider == nil || ct.Provider.Auth.Type == providers.AuthPassthrough {
			return "", ""
		}
		t = ct
	default:
		vt, own, _, err := vendorTargetNote(Request{Model: c.Model}, runner)
		if err != nil || own || vt.Provider == nil || vt.Provider.Auth.Login == runner {
			return "", ""
		}
		t = vt
	}
	return runner + ":" + t.Provider.Name, billingOf(t.Provider)
}

// billingOf: a harness-held key bills per use, anything else is a plan.
func billingOf(p *providers.Provider) string {
	if p.APIKeyAuth() {
		return route.BillCredits
	}
	return route.BillSubscription
}

// ExplicitChain is r alone on the caller's explicit backend (--provider or provider:model), carrying that backend's
// account: a remembered exhaustion skips it without an attempt, and a quota failure is recorded on it.
func ExplicitChain(r Runner, req Request) Chain {
	c := Single(r)
	acct, billing := explicitAccount(r, req)
	c.Legs[0].Account, c.Legs[0].Billing = acct, billing
	if acct == "" {
		return c
	}
	now := time.Now()
	if e := loadState()[acct]; e.Status(now) == route.StateExhausted {
		return Chain{Want: r.Name(), Skipped: []string{r.Name() + ": " + route.ExhaustedReason(acct, e, now)}}
	}
	return c
}

// explicitAccount is "<runner>:<provider>" for the named backend; a hub leg names its own provider.
func explicitAccount(r Runner, req Request) (string, string) {
	if l, ok := r.(hubLeg); ok {
		if vendors.OpencodeCredID(l.provider.OpencodeID) == "opencode" {
			return "opencode:go", route.BillSubscription
		}
		return "opencode:" + l.provider.Name, billingOf(l.provider)
	}
	reg, err := providers.Load()
	if err != nil {
		return "", ""
	}
	name := req.Provider
	if name == "" {
		name, _, _ = strings.Cut(req.Model, ":")
	}
	p, ok := reg.Providers[name]
	if !ok {
		return "", ""
	}
	return r.Name() + ":" + p.Name, billingOf(p)
}

// hubLegFor maps an opencode cell "<opencodeID>/<model>" to the hub leg on that provider and the bare model.
func hubLegFor(reg *providers.Registry, cell string) (Runner, string, error) {
	id, model, ok := strings.Cut(cell, "/")
	if !ok || reg == nil {
		return nil, "", fmt.Errorf("cell %q is not <opencodeID>/<model>", cell)
	}
	for _, name := range reg.Names() {
		if p := reg.Providers[name]; p.OpencodeID == id {
			return hubLeg{agent: "opencode", provider: p, reg: reg}, model, nil
		}
	}
	// opencode's own plans (opencode-go, opencode) have no registry provider: opencode resolves them itself.
	if vendors.OpencodeCredID(id) == "opencode" {
		return hubLeg{agent: "opencode", provider: &providers.Provider{Name: id, OpencodeID: id}, reg: reg}, model, nil
	}
	return nil, "", fmt.Errorf("no provider has opencodeID %q", id)
}

// legRequest is req for leg: the wanted leg keeps an explicit caller model; every other leg drops the original
// provider and session (they belong to the leg that minted them) and runs its cell, the caller's --effort winning.
func legRequest(req Request, leg Leg, first bool) Request {
	out := req
	if leg.unroutedLeg {
		return out
	}
	if !first {
		out.Provider, out.SessionID = "", ""
	}
	if !leg.keepCaller {
		out.Model = leg.Model
	}
	if out.Effort == "" {
		out.Effort = leg.Effort
	}
	return out
}

// noteQuota remembers a KindQuota failure on the leg's account until its parsed reset (else the known window).
func noteQuota(leg Leg, err error) {
	var e *Error
	if leg.Account == "" || !errors.As(err, &e) || e.Kind != KindQuota {
		return
	}
	now := time.Now()
	until := route.ResetFor(err.Error(), leg.Billing, loadState()[leg.Account], now)
	_ = markExhausted(leg.Account, until, now, fallbackReason(err)) // best-effort: the run's own error still reports it
}

// noteStall remembers a stalled free leg for 15 minutes, so the sub-pick skips it instead of hanging again.
func noteStall(leg Leg, err error) {
	if leg.Account == "" || !errors.Is(err, errStalled) {
		return
	}
	now := time.Now()
	reason := fmt.Sprintf("free leg stalled (no output in %s)", firstOutputDeadline())
	_ = markExhausted(leg.Account, now.Add(15*time.Minute), now, reason) // best-effort: the run's own error still reports it
}

// condemnAuth tags a KindAuth whose Recover failed with a condemning verdict, or any error that itself condemns
// (an initial u2m invalid_grant / missing refresh token can classify as fatal with no Recover run).
func condemnAuth(e *Error, recoverErr error) {
	switch {
	case e.Kind == KindAuth && providers.Verdict(recoverErr).Condemns():
		e.AuthReason = fallbackReason(recoverErr)
	case e.Err != nil && providers.Verdict(e.Err).Condemns():
		e.AuthReason = fallbackReason(e.Err)
	}
}

// noteAuth marks the leg's account auth-dead for a condemned KindAuth, so route.Pick skips it until a login clears it.
func noteAuth(leg Leg, err error) {
	var e *Error
	if leg.Account == "" || !errors.As(err, &e) || e.AuthReason == "" {
		return
	}
	_ = markAuth(leg.Account, e.AuthReason) // best-effort: the run's own error still reports it
}
