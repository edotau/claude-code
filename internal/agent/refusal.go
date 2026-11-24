package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// policyNote opens a hop's recorded reason for a policy-refusal re-pick.
const policyNote = "policy refusal: "

// repickPlan is overflowPlan, or one policy-refusal decision: a re-pick to another model family, or why none.
type repickPlan struct {
	overflowPlan
	policy       bool
	pran, pmodel string
	pfrom, pto   string
	prefusal     string
}

// refusedHop is one failed hop a re-pick answers, with the hops recorded so far.
type refusedHop struct {
	disp Dispatch
	req  RunOptions
	res  wire.Result
	err  error
	hops []wire.Hop
}

// planRepick is planOverflow, or for a policy refusal the one re-pick to another family (never a second).
func planRepick(ctx context.Context, in refusedHop) repickPlan {
	ran := ranModel(in.disp, in.req, in.res)
	if ran == "" || in.res.Answer != "" || !retry.IsPolicyRefusal(in.err) || retry.Progressed(in.err) {
		return repickPlan{overflowPlan: planOverflow(in.disp, in.req, in.res, ran, in.err)}
	}
	plan := repickPlan{policy: true, pran: ran, pfrom: models.FamilyName(ran)}
	if policyRepicked(in.hops) || in.req.Repicks > 0 { // the ask's re-pick budget spans its dispatches
		return plan // refusedTwice names both refusals once the loop ends
	}
	ids := policyCandsFn(in.disp, in.req.Tools, ranAccount(in.disp.Account, in.res.Provider), ran)
	switch {
	case explicitModel(ctx, in.req):
		plan.prefusal = fmt.Sprintf("%s (%s) is kept, not replaced; other families: %s", ran, plan.pfrom, namedIDs(ids))
	case len(ids) == 0:
		plan.prefusal = fmt.Sprintf("no model of another family than %s (%s) is available", ran, plan.pfrom)
	default:
		plan.pmodel, plan.pto = ids[0], models.FamilyName(ids[0])
	}
	return plan
}

// explicitModel reports a model the harness never replaces: typed, pinned, no fallback or an explicit runner.
func explicitModel(ctx context.Context, req RunOptions) bool {
	if req.ModelTyped || req.Fallback == router.FallbackNone || req.Fallback == router.FallbackPin {
		return true
	}
	ov, _ := agentconfig.LoadOverlayContext(ctx, "", nil)
	return ov.Runner != ""
}

// policyCandsFn is the other-family models disp's profile may run, best first; a seam for tests.
var policyCandsFn = realPolicyCands

// realPolicyCands is ran's tier pool on disp's profile (rotation rules, served, not held) in another family.
func realPolicyCands(disp Dispatch, tools bool, account, ran string) []string {
	_, row, _ := strings.Cut(account, "·")
	p := loadRegistryWarn().Row(row)
	if p == nil {
		return nil
	}
	name, now := disp.Agent.Name(), time.Now()
	entry := loadState()[account]
	tier := ""
	pool := rotationPool(name, "", p, tools, now)
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} { // deepest tier first: a model in two is the deeper
		if tp := rotationPool(name, alias, p, tools, now); slices.ContainsFunc(tp, func(m string) bool { return models.SameModel(providers.SpellPin(p, m), ran) }) {
			pool, tier = tp, catalog.AliasTier(alias)
			break
		}
	}
	pool = router.ServedPool(p, slices.Clone(pool), now)
	pool = slices.DeleteFunc(pool, func(m string) bool { return router.HeldUntil(entry, name, m, now) != nil })
	other := router.OtherFamily(p, ran, pool)
	if tier != "" {
		cands := make([]catalog.Candidate, len(other))
		for i, m := range other {
			cands[i] = catalog.Candidate{Model: m}
		}
		other = other[:0]
		for _, r := range catalog.Choose(cands, name, tier, tools).Ranked {
			other = append(other, r.Model)
		}
	}
	out := make([]string, len(other))
	for i, m := range other {
		out[i] = providers.SpellPin(p, m)
	}
	return out
}

// namedIDs names up to three ids, or "none" when there are none.
func namedIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids[:min(3, len(ids))], ", ")
}

// note is the re-pick's one line, naming the reason and both families.
func (r repickPlan) note() string {
	if !r.policy {
		return r.overflowPlan.note()
	}
	return fmt.Sprintf("%s%s (%s) → %s (%s)", policyNote, r.pran, r.pfrom, r.pmodel, r.pto)
}

// pick is the re-pick's model, "" when there is none.
func (r repickPlan) pick() string { return r.model + r.pmodel }

// say is the re-pick's note, or other when the plan holds no re-pick.
func (r repickPlan) say(other string) string {
	if r.pick() == "" {
		return other
	}
	return r.note()
}

// refuse is err with the plan's refusal named; err itself when there is none.
func (r repickPlan) refuse(err error) error {
	if r.prefusal == "" {
		return r.overflowPlan.refuse(err)
	}
	return fmt.Errorf("policy refusal, %s: %w", r.prefusal, err) // wraps err itself so errors.Is still finds it
}

// record adds the re-pick's reason and families to the refused hop h.
func (r repickPlan) record(h wire.Hop) wire.Hop {
	if !r.policy {
		return r.overflowPlan.record(h)
	}
	if r.pmodel != "" {
		h.Errors = append(h.Errors, wire.EnvelopeError{Kind: h.ErrorKind, Message: r.note()})
	}
	return h
}

// policyRepicked reports a hop already moved by a policy refusal.
func policyRepicked(hops []wire.Hop) bool { return policyHop(hops) != "" }

// policyHop is the recorded note of the first policy-refusal re-pick in hops, else "".
func policyHop(hops []wire.Hop) string {
	for _, h := range hops {
		for _, e := range h.Errors {
			if strings.HasPrefix(e.Message, policyNote) {
				return e.Message
			}
		}
	}
	return ""
}

// refusedTwice names the earlier re-pick on a policy refusal that followed it; err otherwise.
func refusedTwice(hops []wire.Hop, err error) error {
	first := policyHop(hops)
	if first == "" || !retry.IsPolicyRefusal(err) {
		return err
	}
	return fmt.Errorf("policy refusal twice (one re-pick only), first: %s, then: %w", strings.TrimPrefix(first, policyNote), err)
}

// hopFailed is HopFailed without the provider's should-retry hint.
func hopFailed(res wire.Result, err error) bool {
	if err == nil || res.Answer != "" || requestFault(err) || autoStopped(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	return !retry.Progressed(err)
}

// retryBarred is a transient failure whose provider said a retry cannot help.
func retryBarred(err error) bool {
	e, ok := errors.AsType[*retry.Error](err)
	return ok && e.ShouldRetry == retry.ShouldRetryNo && e.Kind == retry.KindTransient
}

// sameFamilyHop reports next as known to run the model family res ran; an auto or unnamed hop is unknown.
func sameFamilyHop(res wire.Result, next Dispatch) bool {
	asRun := func(m string) string {
		if c, ok := models.Alias(m); ok {
			return c
		}
		return m
	}
	from, to := asRun(res.Model), asRun(next.Model)
	return from != "" && to != "" && !IsAuto(from) && !IsAuto(to) && !router.DifferentFamily(from, to)
}

// barredFall: a should-retry-no transient failure that would otherwise fall through to next.
func barredFall(ctx context.Context, prev Dispatch, res wire.Result, err error) bool {
	return retryBarred(err) && !errors.Is(err, ErrPoolExhausted) && fallsThroughContext(ctx, prev.Agent) && hopFailed(res, err)
}

// barredMove: a barred failure still moves to next unless next is known to run the same family.
func barredMove(ctx context.Context, prev, next Dispatch, res wire.Result, err error) bool {
	return barredFall(ctx, prev, res, err) && !sameFamilyHop(res, next)
}

// barredAhead: a later hop of c takes a barred failure of hop i.
func barredAhead(ctx context.Context, c Chain, i int, res wire.Result, err error) bool {
	return i+1 < len(c.Dispatches) && slices.ContainsFunc(c.Dispatches[i+1:], func(d Dispatch) bool {
		return barredMove(ctx, c.Dispatches[i], d, res, err)
	})
}

// keepsCell: an auto re-pick stays an auto cell; a policy pick is a concrete model.
func (r repickPlan) keepsCell(auto bool) bool { return auto && r.pmodel == "" }

// spends: the re-pick counts against the ask's budget (every auto re-pick, and a policy pick).
func (r repickPlan) spends(auto bool) bool { return auto || r.pmodel != "" }

// skipsSameFamily: the provider said a retry cannot help, so a hop of the same family is passed over for a later one.
func (s *fallbackState) skipsSameFamily(ctx context.Context, next Dispatch) bool {
	return barredFall(ctx, s.prevDispatch, s.res, s.err) && sameFamilyHop(s.res, next)
}

// gate decides hop i before it runs: skip it (same family after a barred failure) or end the chain here.
func (s *fallbackState) gate(ctx context.Context, c Chain, i int, disp Dispatch, midRun bool) (skip, end bool) {
	if i == 0 {
		return false, false
	}
	if s.skipsSameFamily(ctx, disp) {
		return true, false
	}
	// R8: a typed model unserved is never swapped
	return false, s.modelGone() || c.yieldsFallback(s.req, s.res, s.err) || (!midRun && !s.movesOn(ctx, disp))
}
