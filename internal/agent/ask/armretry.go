package ask

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

// A 429 with no Retry-After keeps its workspace out of the pick this long; a longer one is capped at armCoolMax.
const (
	armCoolHold = 30 * time.Second
	armCoolMax  = 5 * time.Minute
)

// armAutoPick reports whether applying the confinement value makes this process pick the workspace (databricks:auto).
func armAutoPick(ctx context.Context, value string) bool {
	provider, p, err := route.Parse(value)
	return err == nil && provider == router.RouteDatabricks && p == route.AutoArm && router.ArmProfileContext(ctx) == ""
}

// armRetry is one foreground ask on an auto-picked workspace; a workspace answering 429 is cooled and the ask moves on.
type armRetry struct {
	agentName, model, fallback, format, save string
	strict                                   bool      // a typed model: a retry only moves to a workspace serving it
	out                                      io.Writer // where the answer and envelope go
	job                                      bool      // a --job child: the answer also lands on stdout, the log
	tried                                    []string  // the workspaces asked so far, in order
	hops                                     []wire.Hop
	record                                   *agent.Record
}

// jobAutoArm reports a --job record whose workspace was an auto pick: its route names the pick, not the intent.
func (r *askRun) jobAutoArm() bool { return r.jobRec != nil && r.jobRec.Request.AutoArm }

// armRun is the retry state of r's run, or nil when it was not an auto pick or cannot move (job, host, resume).
func (r *askRun) armRun() *armRetry {
	f := r.f
	if r.auto == "" || f.host != "" || f.background || f.live || f.session != "" {
		return nil
	}
	return &armRetry{agentName: r.ag, model: f.model, fallback: f.fallback, format: f.format, save: f.save, out: r.out, job: r.jobRec != nil,
		strict: !r.swap && typedModel(f.model), tried: []string{r.auto}}
}

// probe yields pre-progress workspace failures without widening the requested provider group.
func (a *armRetry) probe(req agent.RunOptions) agent.RunOptions {
	req.YieldFallback = func(res wire.Result, err error) bool {
		e, ok := errors.AsType[*retry.Error](err)
		return (errors.Is(err, agent.ErrPoolExhausted) || ok && e.Kind == retry.KindRateLimit) && agent.HopFailed(res, err) &&
			armsLeft(a.agentName, a.model, a.strict, a.tried)
	}
	return req
}

// run executes p, and while its failure is a 429 with another workspace left, re-runs req on the next pick.
func (a *armRetry) run(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions) int {
	a.record = &p.Record
	opts := jobOpts(a.job, a.format, a.out)
	attempt := opts
	attempt.Hold = true
	var held *resultHold
	p, res, err := a.attempts(ctx, cl, p, req, func(ctx context.Context, p *agent.Prepared) (wire.Result, error) {
		sink := sinkTo(sinkOut(a.out, a.format, a.save), a.format, p.Record.ID)
		if a.format == agent.FormatStreamJSON {
			held = &resultHold{Sink: sink}
			sink = held
		}
		return cl.Exec(ctx, p, sink, attempt)
	})
	opts.Notify = cl.Notify
	err = p.Record.Conclude(res, err, opts)
	if held != nil {
		a.emitFinal(held, p, res, err)
	}
	code := reportAsk(a.out, a.format, a.save, p, res, err)
	if prep, ok := errors.AsType[*prepareFailure](err); ok {
		return prepareExitCode(prep.err)
	}
	return code
}

// resultHold keeps an attempt's result event back so only the last attempt's reaches the stream.
type resultHold struct {
	wire.Sink
	ev *wire.Event
}

func (h *resultHold) Emit(ev wire.Event) error {
	if ev.Type != wire.EventResult {
		return h.Sink.Emit(ev)
	}
	h.ev = &ev
	return nil
}

// emitFinal sends the one result event of a stream: the last attempt's own, or the aggregate once the run moved.
func (a *armRetry) emitFinal(h *resultHold, p *agent.Prepared, res wire.Result, err error) {
	ev := h.ev
	if len(a.tried) > 1 || ev == nil {
		env := wire.NewEnvelope(res, err, p.Record.ID)
		env.RanOn = ranOn(res)
		e := wire.ResultEvent(env)
		ev = &e
	}
	if werr := h.Sink.Emit(*ev); werr != nil {
		fmt.Fprintf(os.Stderr, "ask: result event not written to stdout: %v\n", werr)
	}
}

// prepareFailure marks a Prepare error of a retry attempt, so the caller exits with Prepare's code.
type prepareFailure struct{ err error }

func (e *prepareFailure) Error() string { return e.err.Error() }
func (e *prepareFailure) Unwrap() error { return e.err }

// attempts is the askRun-free workspace retry: exec runs one attempt, a 429 re-prepares req on the next pick.
func (a *armRetry) attempts(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions,
	exec func(context.Context, *agent.Prepared) (wire.Result, error)) (*agent.Prepared, wire.Result, error) {
	reserved := false
	for {
		res, err := exec(ctx, p)
		req.Repicks = p.Opts.Repicks
		if reserved && !launched(res) {
			req.Repicks--
		}
		res = a.collect(res, err)
		next, again := a.next(ctx, res, err, req)
		if !again {
			if len(a.tried) > 1 && err != nil {
				err = fmt.Errorf("databricks:auto: tried %s: %w", strings.Join(a.tried, ", "), err)
			}
			return p, res, err
		}
		reserved = next.Repicks > req.Repicks
		req = next
		np, perr := a.prepare(ctx, cl, req, a.record)
		if perr != nil {
			res.Hops = slices.Clone(a.hops)
			return p, res, &prepareFailure{perr}
		}
		p = np
		_ = startPrepared(cl, p, false) // a failed save is warned about
	}
}

func (a *armRetry) collect(res wire.Result, err error) wire.Result {
	if err != nil && len(res.Hops) == 0 {
		res.Hops = []wire.Hop{{Agent: a.agentName, Account: "databricks·" + a.current(),
			Profile: a.current(), ErrorKind: wire.ErrorKind(err), NotStarted: true}}
	}
	for _, h := range res.Hops {
		if profile, ok := strings.CutPrefix(h.Account, "databricks·"); ok && h.Profile == "" {
			h.Profile = profile
		}
		a.hops = append(a.hops, h)
	}
	res.Hops = slices.Clone(a.hops)
	return res
}

func launched(res wire.Result) bool {
	return slices.ContainsFunc(res.Hops, func(h wire.Hop) bool { return !h.NotStarted })
}

// next moves a pre-progress 429 or exhausted pool within the auto-picked provider group.
func (a *armRetry) next(ctx context.Context, res wire.Result, err error, req agent.RunOptions) (agent.RunOptions, bool) {
	if ctx.Err() != nil || !agent.HopFailed(res, err) {
		return req, false
	}
	if errors.Is(err, agent.ErrPoolExhausted) {
		if req.Repicks >= agent.MaxAutoRepicks || !a.moveExhausted() {
			return req, false
		}
		if launched(res) {
			req.Repicks++
		}
		return req, true
	}
	e, ok := errors.AsType[*retry.Error](err)
	return req, ok && e.Kind == retry.KindRateLimit && a.move(e.RetryAfter)
}

// move cools the workspace that answered 429 and picks the next; false when none is left.
func (a *armRetry) move(retryAfter time.Duration) bool {
	cur := a.current()
	coolArm(cur, retryAfter)
	name, err := nextArm(a.agentName, a.model, a.strict, a.tried)
	if err != nil {
		return false
	}
	fmt.Fprintf(os.Stderr, "overlay: databricks:auto: %s rate limited, trying %s\n", cur, name)
	a.tried = append(a.tried, name)
	return true
}

func (a *armRetry) moveExhausted() bool {
	name, err := nextArm(a.agentName, a.model, a.strict, a.tried)
	if err != nil {
		return false
	}
	fmt.Fprintf(os.Stderr, "overlay: databricks:auto: %s pool exhausted, trying %s\n", a.current(), name)
	a.tried = append(a.tried, name)
	return true
}

// coolArm holds workspace name out of the pick for retryAfter (else armCoolHold), the cooling the gateway also writes.
func coolArm(name string, retryAfter time.Duration) {
	reg, err := providers.Load()
	if err == nil && reg.Providers[name] != nil {
		now := time.Now()
		_, err = router.MarkCooling(router.AccountOf(reg.Providers[name]), now.Add(min(cmp.Or(retryAfter, armCoolHold), armCoolMax)), now)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "overlay: databricks:auto: could not cool %s: %v\n", name, err)
	}
}

// nextArm picks outside tried and prints the same receipt as the first dispatch.
func nextArm(agentName, model string, strict bool, tried []string) (string, error) {
	reg, err := providers.Load()
	if err != nil {
		return "", fmt.Errorf("overlay databricks:auto: %w", err)
	}
	return reportPick(pickWith(reg, armQuery{AgentName: agentName, Model: model, Strict: strict, Tried: tried}, time.Now()))
}

// armsLeft reports a workspace outside tried that passes the preflight.
func armsLeft(agentName, model string, strict bool, tried []string) bool {
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	usable, _, _ := router.RankWorkspaces(reg, workspaceQuery(armQuery{AgentName: agentName, Model: model, Strict: strict, Tried: tried}), time.Now())
	return len(usable) > 0
}

// current is the workspace this run is on.
func (a *armRetry) current() string { return a.tried[len(a.tried)-1] }

// armPrepare is the model-routing seam for workspace retry tests.
var armPrepare = (*agent.Client).Prepare

// prepare is cl.Prepare of req on this run's current workspace: its own values, so no other run sees the move.
func (a *armRetry) prepare(ctx context.Context, cl *agent.Client, req agent.RunOptions,
	claim *agent.Record) (*agent.Prepared, error) {
	if a == nil {
		return cl.Prepare(ctx, req, claim)
	}
	if req.Values == nil {
		_, req.Values = agent.RunValues(ctx, nil)
	}
	for {
		req.Values = req.Values.Merge(armValues(a.current()))
		p, err := armPrepare(cl, ctx, req, claim)
		if err == nil {
			return p, nil
		}
		a.hops = append(a.hops, wire.Hop{Agent: a.agentName, Account: "databricks·" + a.current(),
			Profile: a.current(), ErrorKind: wire.ErrorKind(err), NotStarted: true})
		if ctx.Err() != nil || !errors.Is(err, agent.ErrPoolExhausted) || !a.moveExhausted() {
			return nil, err
		}
	}
}

// ranOn is wire.RanOn plus " · tried a → b" when a run moved across Databricks workspaces.
func ranOn(res wire.Result) string {
	line := wire.RanOn(res)
	var tried []string
	for _, h := range res.Hops {
		if strings.HasPrefix(h.Account, "databricks·") && !slices.Contains(tried, h.Profile) {
			tried = append(tried, h.Profile)
		}
	}
	if line == "" || len(tried) < 2 {
		return line
	}
	return line + " · tried " + strings.Join(tried, " → ")
}
