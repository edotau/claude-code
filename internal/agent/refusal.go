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

// policyRefused reports in as a refusal on content grounds a re-pick may answer: not an auto, answered or streamed hop.
func policyRefused(in refusedHop, ran string) bool {
	return ran != "" && !IsAuto(ran) && in.res.Answer == "" && retry.MayRedispatch(in.err) && retry.IsPolicyRefusal(in.err) && !retry.Progressed(in.err)
}

// planRepick is planOverflow, or for a policy refusal the one re-pick to another family (never a second).
func planRepick(ctx context.Context, in refusedHop) repickPlan {
	ran := ranModel(in.disp, in.req, in.res)
	if !policyRefused(in, ran) {
		return repickPlan{overflowPlan: planOverflow(in.disp, in.req, in.res, ran, in.err)}
	}
	plan := repickPlan{policy: true, pran: ran, pfrom: models.FamilyName(ran)}
	ask, _ := ctx.Value(askKey{}).(*askNote)
	if policyRepicked(in.hops) || ask != nil && ask.first != "" { // one re-pick per ask, across its dispatches
		return plan // refusedTwice names both refusals once the loop ends
	}
	ids := policyCandsFn(in.disp, in.req.Tools, ranAccount(in.disp.Account, in.res.Provider), ran)
	tried := askTriedPicks(ctx) // a pick this ask already ran and lost stays out
	ids = slices.DeleteFunc(ids, func(id string) bool {
		return slices.ContainsFunc(tried, func(t string) bool { return models.SameModel(t, id) })
	})
	switch {
	case explicitModel(ctx, in):
		plan.prefusal = fmt.Sprintf("%s (%s) is kept, not replaced; other families: %s", ran, plan.pfrom, namedIDs(ids))
	case len(ids) == 0:
		plan.prefusal = fmt.Sprintf("no model of another family than %s (%s) is available", ran, plan.pfrom)
	default:
		plan.pmodel, plan.pto = ids[0], models.FamilyName(ids[0])
	}
	return plan
}

// askKey carries what the whole ask said, past the per-hop request rewrites.
type askKey struct{}

// askNote is the ask's typed-model flag and its one policy re-pick's note.
type askNote struct {
	typed bool
	first string
}

// withAsk is ctx carrying req's own typed-model flag and the ask's policy re-pick budget.
func withAsk(ctx context.Context, req RunOptions) context.Context {
	return context.WithValue(ctx, askKey{}, &askNote{typed: req.ModelTyped})
}

// explicitModel reports a model the harness never replaces: typed, pinned, no fallback or an explicit runner.
func explicitModel(ctx context.Context, in refusedHop) bool {
	req := in.req
	if ask, ok := ctx.Value(askKey{}).(*askNote); ok && ask.typed || req.ModelTyped {
		return true
	}
	if req.Fallback == router.FallbackNone || req.Fallback == router.FallbackPin {
		return true
	}
	if pin := agentModelPinContext(valuesContext(ctx, req.Values), in.disp.agentName()); pin != "" && !IsAuto(pin) && !IsAuto(req.Model) && !req.AutoModel {
		return true
	}
	ov, _ := agentconfig.LoadOverlayContext(valuesContext(ctx, req.Values), "", nil)
	return ov.Runner != ""
}

// policyTiers lists the dispatch tiers deepest first with the route alias whose rotation pool serves each.
var policyTiers = []struct{ tier, alias string }{{"deep", models.Opus}, {"standard", models.Sonnet}, {"fast", models.Haiku}}

// refusedTier is the deepest tier whose capability floor ran's rank meets; "" for a model no rank covers.
func refusedTier(ran string) (tier, alias string) {
	r := catalog.RankOf(ran)
	for _, t := range policyTiers {
		if r.Known && r.Capability >= catalog.Choose(nil, "", t.tier, false).Need {
			return t.tier, t.alias
		}
	}
	return "", ""
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
	tier, alias := refusedTier(ran)
	if tier == "" {
		return nil // an unranked model has no tier to keep
	}
	pool := router.ServedPool(p, slices.Clone(rotationPool(name, alias, p, tools, now)), now)
	pool = slices.DeleteFunc(pool, func(m string) bool { return router.HeldUntil(entry, name, m, now) != nil })
	other := router.OtherFamily(p, ran, pool)
	cands := make([]catalog.Candidate, len(other))
	for i, m := range other {
		cands[i] = catalog.Candidate{Model: m, Tools: "yes"} // rotationPool already applied the tools probe
	}
	other = other[:0]
	for _, r := range catalog.Choose(cands, name, tier, tools).Ranked {
		other = append(other, r.Model)
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
	return policyWrap(err, "policy refusal, "+r.prefusal)
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
func refusedTwice(ctx context.Context, hops []wire.Hop, err error) error {
	first := policyHop(hops)
	if ask, ok := ctx.Value(askKey{}).(*askNote); first == "" && ok {
		first = ask.first
	}
	if first == "" || !retry.IsPolicyRefusal(err) {
		return err
	}
	return policyWrap(err, "policy refusal twice (one re-pick only), first: "+strings.TrimPrefix(first, policyNote)+", then")
}

// policyWrap is err led by msg, inside a copy's Err for a retry.Error so prefixErr keeps it; errors.Is finds err.
func policyWrap(err error, msg string) error {
	e, ok := errors.AsType[*retry.Error](err)
	if !ok {
		return fmt.Errorf("%s: %w", msg, err)
	}
	out := *e
	out.Err = fmt.Errorf("%s: %w", msg, err)
	return &out
}

// keepsCell: an auto re-pick stays an auto cell; a policy pick is a concrete model.
func (r repickPlan) keepsCell(auto bool) bool { return auto && r.pmodel == "" }

// spend counts a started re-pick against the ask's budget: every auto re-pick, and the ask's one policy pick.
func (r repickPlan) spend(ctx context.Context, auto bool) bool {
	if ask, ok := ctx.Value(askKey{}).(*askNote); ok && r.pmodel != "" {
		ask.first = r.note()
	}
	return auto || r.pmodel != ""
}
