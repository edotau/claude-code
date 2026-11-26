package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

// LocalHop is the last-resort row a plan may end on, Barred with why falling to it would be wrong.
type LocalHop struct {
	Workspace Workspace
	Barred    string `json:",omitempty"`
}

// localRules is the part of ranks.json's local block the plan reads.
type localRules struct {
	Stage     string
	Tiers     []string
	MaxPrompt int
}

// localRuleOf reads catalog.Local; a seam so tests set the stage.
var localRuleOf = func() localRules {
	r := catalog.Local()
	return localRules{Stage: r.Stage, Tiers: r.Tiers, MaxPrompt: r.MaxPrompt}
}

// hop is h as the plan's last hop, pi on the row; false when nil, barred, or the stage is not last.
func (h *LocalHop) hop(agent string) (PlanHop, bool) {
	if h == nil || h.Barred != "" || localRuleOf().Stage != "last" {
		return PlanHop{}, false
	}
	return PlanHop{Runner: Runner{Kind: RunnerPi, Agent: agent}, Workspace: h.Workspace}, true
}

// admitLocal is f plus profile when it is p's admitted last-resort row: only the local hop's token may reach it.
func (p RoutePlan) admitLocal(f route.Fence, profile string) route.Fence {
	if h, ok := p.Local.hop(""); ok && h.Workspace.Profile == profile && !f.Open {
		f.Profiles = append(slices.Clone(f.Profiles), profile)
	}
	return f
}

// localFenced is f, widened to the local hop's own row when profiles is that row alone.
func localFenced(ctx context.Context, f route.Fence, profiles []string) route.Fence {
	if plan, ok := PlanOf(ctx); ok && len(profiles) == 1 {
		return plan.admitLocal(f, profiles[0])
	}
	return f
}

// carries reports d as h's admitted hop: it bills the last-resort row's account.
func (h *LocalHop) carries(d Dispatch) bool {
	_, ok := h.hop("")
	return ok && d.Account == h.Workspace.Account
}

// planLocal is reg's last-resort row as p's local hop, Barred with the first reason against it; nil with no row.
func planLocal(_ context.Context, reg *providers.Registry, o RunOptions, p RoutePlan) *LocalHop {
	row := providers.LastResort(reg)
	rule := localRuleOf()
	if row == nil || rule.Stage == "off" {
		return nil
	}
	ws := Workspace{Profile: row.Name, Account: router.AccountOf(row)}
	return &LocalHop{Workspace: ws, Barred: cmp.Or(policyBar(o, p, row.Name), requestBar(o, row.Name), briefBar(o, p, rule))}
}

// policyBar is why p's fallback or fence keeps the run off row; a pin or fence bars it only when typed on this command.
func policyBar(o RunOptions, p RoutePlan, row string) string {
	switch {
	case p.Fallback.Kind == router.PolicyNone:
		return "--fallback none"
	case p.Fallback.Kind == router.PolicySameWorkspace && !p.Fallback.Agents && p.Fallback.Typed():
		return "--fallback " + p.Fallback.Mode() + " keeps the planned workspace"
	case o.FenceTyped && !p.Fence.Allows(row):
		return "the fence does not allow " + row
	}
	return ""
}

// pinsBackend is explicitBackend, or a whole Route a batch line's overlay named without a profile.
func pinsBackend(o RunOptions) bool { return explicitBackend(o) || !o.Route.IsZero() }

// requestBar is why o itself must not be swapped onto row: a typed model (R8) or a run tied to its own CLI.
func requestBar(o RunOptions, row string) string {
	switch {
	case pinsBackend(o):
		return "--provider pins the backend"
	case o.ModelTyped && o.Model != "" && !IsAuto(o.Model):
		return "a typed model is never swapped (R8)"
	case o.Native:
		return "--native"
	case o.Host != "":
		return "--host"
	case o.Identity != "":
		return "--as"
	case o.SessionID != "":
		return "a vendor session"
	case cmp.Or(typedProfile(o), o.Lead) == row:
		return "the lead is " + row
	}
	return ""
}

// briefBar is why the brief is beyond the local model: its typed tier, a security review (R17), or its size.
func briefBar(o RunOptions, p RoutePlan, rule localRules) string {
	tier := rotationTiers[p.Tier.Asked] // an untiered brief names no tier to bar
	switch {
	case tier != "" && !slices.Contains(rule.Tiers, tier):
		return "tier " + tier + " is not in local.tiers"
	case o.Rubric != "" && securityBrief().MatchString(o.Prompt):
		return "a security review (R17)"
	}
	return promptBar(o.Prompt, rule)
}

// promptBar is why prompt, as sent, is over the local model's token cap.
func promptBar(prompt string, rule localRules) string {
	if tokens := len(prompt) / 4; tokens > rule.MaxPrompt {
		return fmt.Sprintf("prompt ~%d tokens is over local.max_prompt %d", tokens, rule.MaxPrompt)
	}
	return ""
}

// securityBrief is the R17 topic list: a review naming one of them stays on a hosted model.
var securityBrief = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(secrets?|guard|ssh|sync|exfiltrat\w*)\b`)
})

// withLocal ends ch with p's local hop: pi on the last-resort row, never the asked agent's own CLI.
func withLocal(ch Chain, p RoutePlan, o RunOptions) Chain {
	h, ok := p.Local.hop(p.Agent)
	if !ok || len(ch.Dispatches) == 0 {
		return ch
	}
	pi, err := Lookup(agentid.Pi)
	if err != nil || pi.Available() != nil {
		return ch.noted("local: skipped: pi is not installed")
	}
	reg := loadRegistryWarn()
	row := reg.Row(h.Workspace.Profile)
	if row == nil {
		return ch
	}
	d, why := piHop(reg, router.ChainHop{Agent: agentid.Pi, Profile: row.Name, Account: h.Workspace.Account,
		Model: row.Models[models.Sonnet]}, cmp.Or(o.Effort, "low"))
	if why != "" {
		return ch.noted("local: skipped: " + why)
	}
	d.Agent, d.fixedModel = pi, true
	if p.Agent != agentid.Pi {
		d.runFor = p.Agent
	}
	ch.Dispatches = append(slices.Clone(ch.Dispatches), d)
	return ch
}

// localHop is the planLocal hop a chain carries in ctx while it walks the hops before it.
type localHop struct{}

// splitLocal takes c's local hop off its end into ctx, so the walk and its end-of-chain rules never see it.
func splitLocal(ctx context.Context, c Chain) (Chain, context.Context) {
	n := len(c.Dispatches)
	if n < 2 || c.Plan == nil || !c.Plan.Local.carries(c.Dispatches[n-1]) {
		return c, ctx
	}
	d := c.Dispatches[n-1]
	c.Dispatches = c.Dispatches[:n-1]
	return c, context.WithValue(ctx, localHop{}, d)
}

// isLocalHop reports disp as the local hop ctx carries.
func isLocalHop(ctx context.Context, disp Dispatch) bool {
	d, ok := ctx.Value(localHop{}).(Dispatch)
	return ok && d.Account == disp.Account
}

// localKind is the failure's Kind and whether the local hop takes it over: pre-answer, not the task's fault.
func localKind(res wire.Result, err error) (retry.Kind, bool) {
	e, ok := errors.AsType[*retry.Error](err)
	if !ok || !HopFailed(res, err) {
		return retry.KindNone, false
	}
	if e.AuthReason != "" {
		return retry.KindAuth, true
	}
	switch e.Kind { // a context overflow gets here only when planLocal found the prompt under local.max_prompt
	case retry.KindTransient, retry.KindAuth, retry.KindRateLimit, retry.KindQuota, retry.KindContext:
		return e.Kind, true
	}
	return e.Kind, false
}

// localFollows reports hop i of c failing in a way the chain's local hop takes over, so fail-fast must not trip.
func localFollows(ctx context.Context, c Chain, i int, res wire.Result, err error) bool {
	_, has := ctx.Value(localHop{}).(Dispatch)
	_, falls := localKind(res, err)
	return has && falls && i < len(c.Dispatches)
}

// runLocal runs ctx's local hop after c when c's last failure falls to it; a down or busy local keeps that failure.
func (s *fallbackState) runLocal(ctx context.Context, c Chain, sink wire.Sink, notify func(string)) error {
	d, has := ctx.Value(localHop{}).(Dispatch)
	kind, falls := localKind(s.res, s.err)
	switch {
	case !falls || c.yieldsFallback(s.req, s.res, s.err) || ctx.Err() != nil || pinsBackend(s.req):
		return nil
	case !has:
		noteLocalBar(c, notify)
		return nil
	}
	waited := time.Now()
	slot, ok := s.admitFirst(ctx, notify)
	if !ok {
		return nil
	}
	defer slot()
	if s.req.Timeout > 0 {
		s.req.Timeout = max(time.Millisecond, s.req.Timeout-time.Since(waited))
	}
	keep, i := *s, len(c.Dispatches)
	if herr := s.handOff(i, d, false, sink, notify); herr != nil {
		return herr
	}
	s.stopped = s.dispatch(ctx, c, i, d, sink, notify)
	if s.err != nil && s.res.NotStarted { // down or busy, said once by admitLocal: the earlier failure is the result
		*s = keep
		return nil
	}
	s.fell = kind
	return nil
}

// admitFirst bars an over-cap prompt as sent (rubric included), then takes the local slot within req.Timeout.
func (s *fallbackState) admitFirst(ctx context.Context, notify func(string)) (func(), bool) {
	if why := promptBar(s.req.Prompt, localRuleOf()); why != "" {
		if notify != nil {
			notify("local: barred (" + why + ")")
		}
		return nil, false
	}
	if s.req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.req.Timeout)
		defer cancel()
	}
	slot, err := localGate(ctx, notify)
	return slot, err == nil
}

// noteLocalBar says once why a failure that would fall to local at stage last did not.
func noteLocalBar(c Chain, notify func(string)) {
	if notify != nil && c.Plan != nil && c.Plan.Local != nil && c.Plan.Local.Barred != "" && localRuleOf().Stage == "last" {
		notify("local: barred (" + c.Plan.Local.Barred + ")")
	}
}

// localAnswered stamps the Kind the local hop took over on an answered res and notes the fallback.
func (s *fallbackState) localAnswered(res wire.Result, err error) wire.Result {
	if s.fell == retry.KindNone || err != nil {
		return res
	}
	res.FallbackKind = s.fell.String()
	noteLocalFallback(res.FallbackKind, "dispatch")
	return res
}

// Local slot and note seams: tests keep the real probe, slot and state file out.
var (
	localGate         = admitLocal
	noteLocalFallback = router.NoteLocalFallback
)

// localWait bounds how long the local hop waits for the one local slot.
const localWait = 120 * time.Second

// admit takes the account's permit under req.Timeout and leaves req.Timeout as what the attempt has left.
func admit(ctx context.Context, key string, req *RunOptions) (func(), error) {
	started := time.Now()
	release, err := retry.AcquireDispatch(retry.WithAdmissionTimeout(ctx, req.Timeout), key)
	if req.Timeout > 0 {
		req.Timeout = max(time.Millisecond, req.Timeout-time.Since(started))
	}
	return release, err
}

// admitHop is admit; the local hop (past the chain's end) is slimmed first, its slot already held by runLocal.
func admitHop(ctx context.Context, past bool, key string, req *RunOptions) (func(), error) {
	if d, has := ctx.Value(localHop{}).(Dispatch); has && past {
		*req = slimForLocal(d, *req)
	}
	return admit(ctx, key, req)
}

// admitLocal probes the last-resort row and takes its slot, saying once when it waits; a refusal is the hop's error.
func admitLocal(ctx context.Context, notify func(string)) (func(), error) {
	say := func(line string) {
		if notify != nil {
			notify("local: " + line)
		}
	}
	reg, err := providers.Load()
	if err != nil {
		return func() {}, err
	}
	if up, why := router.LocalUp(ctx, reg); !up {
		say("down (" + why + ")")
		return func() {}, errors.New("local model down: " + why)
	}
	slot, err := router.LocalAdmit(ctx, 0)
	if err != nil {
		say(err.Error() + "; waiting up to " + localWait.String())
		slot, err = router.LocalAdmit(ctx, localWait)
	}
	if err != nil {
		say(err.Error())
	}
	return slot, err
}

// slimForLocal is o for the local hop: no review-loop block, d's effort (low unless typed), at most the tools timeout.
func slimForLocal(d Dispatch, o RunOptions) RunOptions {
	o.Prompt, o.ReviewLoop, o.Effort = WithoutReviewLoop(o.Prompt), false, cmp.Or(d.Effort, "low")
	if o.Timeout <= 0 || o.Timeout > DefaultToolsTimeout {
		o.Timeout = DefaultToolsTimeout
	}
	return o
}
