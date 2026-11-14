package ask

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// A 429 with no Retry-After keeps its workspace out of the pick this long; a longer one is capped at armCoolMax.
const (
	armCoolHold = 30 * time.Second
	armCoolMax  = 5 * time.Minute
)

// askRunner runs the prepared foreground dispatch p (built from req) and reports it; the exit code.
type askRunner func(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions) int

// armAutoPick reports whether applying the confinement value makes this process pick the workspace (databricks:auto).
func armAutoPick(value string) bool {
	provider, p, err := router.ParseCoworker(value)
	return err == nil && provider == router.RouteDatabricks && p == router.AutoArm && router.ArmProfile() == ""
}

// armRetry is one foreground ask on an auto-picked workspace; a workspace answering 429 is cooled and the ask moves on.
type armRetry struct {
	agentName, model, fallback, format string
	out                                io.Writer // where the answer and envelope go
	job                                bool      // a --job child: the answer also lands on stdout, the log
	tried                              []string  // the workspaces asked so far, in order
}

// jobAutoArm reports a --job record whose workspace was an auto pick: its route names the pick, not the intent.
func (r *askRun) jobAutoArm() bool { return r.jobRec != nil && r.jobRec.Request.AutoArm }

// armRun is the retry state of r's run, or nil when it was not an auto pick or cannot move (job, host, resumed session).
func (r *askRun) armRun() *armRetry {
	f := r.f
	if r.auto == "" || f.host != "" || f.background || f.live || f.session != "" {
		return nil
	}
	return &armRetry{agentName: r.ag, model: f.model, fallback: f.fallback, format: f.format, out: r.out, job: r.jobRec != nil,
		tried: []string{r.auto}}
}

// probe keeps the requested chain, yielding only a pre-progress 429 to another usable workspace.
func (a *armRetry) probe(req agent.RunOptions) agent.RunOptions {
	req.YieldFallback = func(res wire.Result, err error) bool {
		e, ok := errors.AsType[*retry.Error](err)
		return ok && e.Kind == retry.KindRateLimit && agent.HopFailed(res, err) &&
			armsLeft(a.agentName, a.model, a.tried)
	}
	return req
}

// run executes p, and while its failure is a 429 with another workspace left, re-runs req on the next pick.
func (a *armRetry) run(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions) int {
	opts := jobOpts(a.job, a.format, a.out)
	attempt := opts
	attempt.Hold = a.job // a job keeps one record: only the last attempt concludes it
	var held *resultHold
	p, res, err := a.attempts(ctx, cl, p, req, func(ctx context.Context, p *agent.Prepared) (wire.Result, error) {
		sink := sinkTo(a.out, a.format, p.Record.ID)
		if a.format == agent.FormatStreamJSON {
			held = &resultHold{Sink: sink}
			sink = held
		}
		return cl.Exec(ctx, p, sink, attempt)
	})
	if a.job {
		opts.Notify = cl.Notify
		p.Record.Conclude(res, err, opts)
	}
	if prep, ok := errors.AsType[*prepareFailure](err); ok {
		return failAskCode(a.format, a.agentName, prep.err, prepareExitCode(prep.err))
	}
	if held != nil {
		a.emitFinal(held, p, res, err)
	}
	return reportAsk(a.out, a.format, p, res, err)
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
	var hops []wire.Hop
	for {
		res, err := a.exec(ctx, p, exec)
		hops = append(hops, res.Hops...)
		res.Hops = slices.Clone(hops)
		next, again := a.next(ctx, res, err, req)
		if !again {
			if len(a.tried) > 1 && err != nil {
				err = fmt.Errorf("databricks:auto: tried %s: %w", strings.Join(a.tried, ", "), err)
			}
			return p, res, err
		}
		req = next
		np, perr := a.prepare(ctx, cl, req, a.claimOf(p))
		if perr != nil {
			return p, res, &prepareFailure{perr}
		}
		p = np
		_ = startPrepared(cl, p, false) // a failed save is warned about
	}
}

// claimOf is the record the next attempt continues: a --job run keeps its one published record, the rest start anew.
func (a *armRetry) claimOf(p *agent.Prepared) *agent.Record {
	if !a.job {
		return nil
	}
	return &p.Record
}

// next moves a pre-progress 429 to another workspace; all other outcomes finish the requested chain.
func (a *armRetry) next(ctx context.Context, res wire.Result, err error, req agent.RunOptions) (agent.RunOptions, bool) {
	if ctx.Err() != nil || !agent.HopFailed(res, err) {
		return req, false
	}
	if e, ok := errors.AsType[*retry.Error](err); ok && e.Kind == retry.KindRateLimit {
		if moved, ok := a.move(ctx, req, e.RetryAfter); ok {
			return moved, true
		}
	}
	return req, false
}

// move cools the workspace that answered 429 and exports the next pick; false when none is left.
func (a *armRetry) move(ctx context.Context, req agent.RunOptions, retryAfter time.Duration) (agent.RunOptions, bool) {
	cur := a.tried[len(a.tried)-1]
	coolArm(cur, retryAfter)
	if enterArm(ctx, cur) != nil { // the pick reads the process arm: hold it on this run's own workspace
		return req, false
	}
	defer leaveArm()
	name, _, err := nextArm(a.agentName, a.model, a.tried)
	if err != nil {
		return req, false
	}
	fmt.Fprintf(os.Stderr, "overlay: databricks:auto: %s rate limited, trying %s\n", cur, name)
	a.tried = append(a.tried, name)
	return req, true
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

// nextArm is pickArm outside tried, printed like the first pick; more: another workspace is left behind it.
func nextArm(agentName, model string, tried []string) (name string, more bool, err error) {
	reg, err := providers.Load()
	if err != nil {
		return "", false, fmt.Errorf("overlay databricks:auto: %w", err)
	}
	now := time.Now()
	p, live, skipped, err := pickArm(reg, agentName, model, now, tried...)
	if name, err = reportPick(p, live, skipped, err); err != nil {
		return "", false, err
	}
	rest, _ := usableArms(reg, agentName, model, now, append(slices.Clone(tried), name))
	return name, len(rest) > 0, nil
}

// armsLeft reports a workspace outside tried that passes the preflight.
func armsLeft(agentName, model string, tried []string) bool {
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	usable, _ := usableArms(reg, agentName, model, time.Now(), tried)
	return len(usable) > 0
}

// armCandidates splits reg's Databricks workspaces (no gateway row, none in tried) by armFindings: usable, skipped.
func armCandidates(reg *providers.Registry, agentName, model string, now time.Time, tried []string) (usable []*providers.Provider, skipped []agent.Finding) {
	for _, n := range slices.Sorted(maps.Keys(reg.Providers)) {
		p := reg.Providers[n]
		if p.Preset != providers.PresetDatabricks || p.IsAIGateway() || slices.Contains(tried, n) {
			continue
		}
		if fs := armFindings(p, agentName, model, now); len(fs) > 0 {
			skipped = append(skipped, fs[0])
			continue
		}
		usable = append(usable, p)
	}
	return usable, skipped
}

// armGate holds the process-wide arm steady: runs on one workspace share it, runs on another wait their turn in order.
var armGate = struct {
	cond  *sync.Cond
	cur   string
	n     int
	queue []*armTicket // the entrants not yet in, oldest first
}{cond: sync.NewCond(new(sync.Mutex))}

// armTicket is one waiting enterArm.
type armTicket struct{ arm string }

// canEnter reports whether t may join: the gate is idle or on its workspace, and no older waiter wants another one.
func canEnter(t *armTicket) bool {
	g := &armGate
	if g.n > 0 && g.cur != t.arm {
		return false
	}
	for _, w := range g.queue {
		if w == t {
			return true
		}
		if w.arm != t.arm {
			return false
		}
	}
	return true
}

// enterArm waits until the process arm may be arm (or ctx ends: ctx.Err()), sets it and counts in; pair with leaveArm.
func enterArm(ctx context.Context, arm string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g := &armGate
	defer context.AfterFunc(ctx, func() {
		g.cond.L.Lock()
		g.cond.Broadcast()
		g.cond.L.Unlock()
	})()
	g.cond.L.Lock()
	defer g.cond.L.Unlock()
	t := &armTicket{arm: arm}
	g.queue = append(g.queue, t)
	defer func() {
		g.queue = slices.DeleteFunc(g.queue, func(w *armTicket) bool { return w == t })
		g.cond.Broadcast()
	}()
	for !canEnter(t) {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	if g.n == 0 {
		if err := setArm(arm); err != nil {
			return err
		}
		g.cur = arm
	}
	g.n++
	return nil
}

// leaveArm releases one enterArm.
func leaveArm() {
	g := &armGate
	g.cond.L.Lock()
	g.n--
	g.cond.L.Unlock()
	g.cond.Broadcast()
}

// current is the workspace this run is on.
func (a *armRetry) current() string { return a.tried[len(a.tried)-1] }

// armPrepare is the model-routing seam for workspace retry tests.
var armPrepare = (*agent.Client).Prepare

// prepare is cl.Prepare with the process arm held on this run's current workspace.
func (a *armRetry) prepare(ctx context.Context, cl *agent.Client, req agent.RunOptions,
	claim *agent.Record) (*agent.Prepared, error) {
	if err := enterArm(ctx, a.current()); err != nil {
		return nil, err
	}
	defer leaveArm()
	return armPrepare(cl, ctx, req, claim)
}

// exec is one attempt with the process arm held on this run's current workspace.
func (a *armRetry) exec(ctx context.Context, p *agent.Prepared,
	run func(context.Context, *agent.Prepared) (wire.Result, error)) (wire.Result, error) {
	if err := enterArm(ctx, a.current()); err != nil {
		return wire.Result{Agent: a.agentName, Status: wire.StatusError}, err
	}
	defer leaveArm()
	return run(ctx, p)
}

// setArm moves this process's confinement to workspace name, as applyArm exports it.
func setArm(name string) error {
	if err := exportConfinement(router.RouteDatabricks, name); err != nil {
		return err
	}
	return proc.Setenv(providers.EnvSessionProfile, name)
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
