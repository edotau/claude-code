package ask

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
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
	job                                bool     // a --job child: the answer also lands on stdout, the log
	tried                              []string // the workspaces asked so far, in order
}

// jobAutoArm reports a --job record whose workspace was an auto pick: its route names the pick, not the intent.
func (r *askRun) jobAutoArm() bool { return r.jobRec != nil && r.jobRec.Request.AutoArm }

// armRun is the retry state of r's run, or nil when it was not an auto pick or cannot move (job, host, resumed session).
func (r *askRun) armRun() *armRetry {
	f := r.f
	if r.auto == "" || f.host != "" || f.background || f.live || f.session != "" {
		return nil
	}
	return &armRetry{agentName: r.ag, model: f.model, fallback: f.fallback, format: f.format, job: r.jobRec != nil,
		tried: []string{r.auto}}
}

// probe is req for the first attempt: fallback none while another workspace is left, so a 429 moves workspaces first.
func (a *armRetry) probe(req agent.RunOptions) agent.RunOptions {
	if a.fallback != router.FallbackNone && armsLeft(a.agentName, a.model, a.tried) {
		req.Fallback = router.FallbackNone
	}
	return req
}

// run executes p, and while its failure is a 429 with another workspace left, re-runs req on the next pick.
func (a *armRetry) run(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions) int {
	var hops []wire.Hop
	opts := agent.RunRecordedOpts{Job: a.job}
	if a.job && a.format == helptext.Text {
		opts.AnswerTo = os.Stdout
	}
	for {
		res, err := cl.Exec(ctx, p, stdoutSink(a.format, p.Record.ID), opts)
		hops = append(hops, res.Hops...)
		res.Hops = slices.Clone(hops)
		next, again := a.next(ctx, res, err, req)
		if !again {
			if len(a.tried) > 1 && err != nil {
				err = fmt.Errorf("databricks:auto: tried %s: %w", strings.Join(a.tried, ", "), err)
			}
			return reportAsk(a.format, p, res, err)
		}
		req = next
		if p, err = cl.Prepare(ctx, req, nil); err != nil {
			return failAskCode(a.format, a.agentName, err, prepareExitCode(err))
		}
		_ = startPrepared(cl, p, false) // a failed save is warned about
	}
}

// next is the request for the attempt after a failed one (next workspace on a 429, else the skipped policy); false: final.
func (a *armRetry) next(ctx context.Context, res wire.Result, err error, req agent.RunOptions) (agent.RunOptions, bool) {
	if ctx.Err() != nil || !agent.HopFailed(res, err) {
		return req, false
	}
	if e, ok := errors.AsType[*retry.Error](err); ok && e.Kind == retry.KindRateLimit {
		if moved, ok := a.move(req, e.RetryAfter); ok {
			return moved, true
		}
	}
	if req.Fallback == a.fallback {
		return req, false
	}
	req.Fallback = a.fallback // the probe ran with fallback none: the policy now applies to the same workspace
	return req, true
}

// move cools the workspace that answered 429 and exports the next pick; false when none is left.
func (a *armRetry) move(req agent.RunOptions, retryAfter time.Duration) (agent.RunOptions, bool) {
	cur := a.tried[len(a.tried)-1]
	coolArm(cur, retryAfter)
	name, more, err := nextArm(a.agentName, a.model, a.tried)
	if err != nil || setArm(name) != nil {
		return req, false
	}
	fmt.Fprintf(os.Stderr, "overlay: databricks:auto: %s rate limited, trying %s\n", cur, name)
	a.tried = append(a.tried, name)
	if req.Fallback = a.fallback; more {
		req.Fallback = router.FallbackNone
	}
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

// setArm moves this process's confinement to workspace name, as applyArm exports it.
func setArm(name string) error {
	if err := exportConfinement(router.RouteDatabricks, name); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, name)
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
