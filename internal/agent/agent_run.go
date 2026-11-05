package agent

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// Run drops unsupported fields loudly, then runs r under DefaultPolicy with req.Timeout per attempt.
func Run(ctx context.Context, r Engine, req RunOptions, sink Sink, notify func(string)) (Result, error) {
	if sink == nil {
		sink = Discard
	}
	var err error
	req.Permissions, err = inheritPermissions(req.Permissions)
	if err == nil {
		err = RefusePersona(r.Caps(), r.Name(), req.Persona)
	}
	if err == nil {
		err = RefuseReadOnly(r.Caps(), r.Name(), req.ReadOnly)
	}
	if err == nil {
		err = RefusePermissions(r.Caps(), r.Name(), req.Permissions)
	}
	if err != nil {
		return Result{Agent: r.Name(), Status: StatusError}, &Error{Kind: KindFatal, Agent: r.Name(), Err: err}
	}
	req, dropped, emulated := applyCaps(r.Caps(), req)
	ctx = withPermissionsEnv(ctx, req.Permissions)
	res := Result{Agent: r.Name(), Status: StatusError, Dropped: dropped, Emulated: emulated}
	// a no-tools one-shot answers from the provider's API, so it needs no agent binary up front
	if !req.NoTools {
		if err := r.Available(); err != nil {
			return res, &Error{Kind: KindFatal, Agent: r.Name(), Err: err}
		}
	}
	p := runPolicy(ctx, req, notify)
	var recoverErr error
	if _, t, err := resolveTarget(req); err == nil && (t.Provider.Auth.Command != "" || t.Provider.Auth.Sealed()) {
		p.Recover = func(ctx context.Context) error {
			_, recoverErr = providers.Refresh(ctx, t.Provider)
			return recoverErr
		}
	}
	start := time.Now()
	state := runState{res: res, req: req, dropped: dropped, emulated: emulated}
	e := Do(ctx, p, func(actx context.Context, attempt int) *Error {
		state.attempts = attempt
		return state.attempt(ctx, actx, r, sink, notify)
	})
	return state.finish(r.Name(), start, e, recoverErr)
}

type fallbackState struct {
	req          RunOptions
	res          Result
	err          error
	prev         Engine
	prevDispatch Dispatch
	reasons      []string
	ran          int
	stopped      bool
	handoff      string
	hops         []Hop
}

func (s *fallbackState) step(ctx context.Context, c Chain, i int, disp Dispatch, sink Sink, notify func(string)) (bool, error) {
	// An answer or final failure ends the chain before any cancel check.
	if i > 0 && !dispatchUnavailable(s.prev, s.res, s.err) {
		s.stopped = true
		return true, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		ce := &Error{Kind: KindFatal, Agent: disp.Agent.Name(), Err: cerr}
		if i == 0 {
			s.res = Result{Agent: ce.Agent, Status: statusOf(ce)}
			return true, ce
		}
		s.err, s.res.Status, s.stopped = ce, statusOf(ce), true
		return true, nil
	}
	if i > 0 {
		s.reasons = append(s.reasons, hopLabel(s.prevDispatch)+": "+FallbackReason(s.err))
		if herr := announceFallback(sink, notify, s.prev, disp, s.err); herr != nil {
			return true, herr
		}
	}
	panicked := s.dispatch(ctx, c, i, disp, sink, notify)
	// Bookkeeping precedes panic handling so a panicking fallback is still reported as the fallback.
	if panicked {
		s.stopped = true
		return true, nil
	}
	if i == 0 && ExplicitBackend(s.req) {
		s.stopped = true
		return true, nil
	}
	return false, nil
}

func (s *fallbackState) dispatch(ctx context.Context, c Chain, i int, disp Dispatch, sink Sink, notify func(string)) bool {
	lreq := dispatchRequest(s.req, disp, i == 0 && disp.Agent.Name() == c.Want)
	ctx, disp = freezeCopilot(ctx, disp, lreq) // The gate's actual target precedes the permit and cooldown key.
	disp = ownLoginActual(disp, lreq)
	key := cmp.Or(disp.Account, disp.Agent.Name()+"·own")
	waitCooldown(ctx, key, DefaultMaxDelay)
	var dhops []Hop
	var panicked bool
	s.res, dhops, panicked, s.err = runDispatch(ctx, c, i, disp, lreq, s.req, key, sink, notify)
	s.hops = append(s.hops, dhops...)
	if s.res.Handoff != s.handoff {
		s.req.Prompt = promptFromHandoff(s.res.Handoff, s.req.Prompt)
	}
	s.handoff = cmp.Or(s.res.Handoff, s.handoff)
	s.prev, s.prevDispatch, s.ran = disp.Agent, disp, i
	return panicked
}

type runState struct {
	res               Result
	req               RunOptions
	dropped, emulated []string
	attempts          int
	failed            []*Error
}

func (s *runState) note(e *Error) *Error {
	s.failed = append(s.failed, withoutEarlier(e))
	return e
}

func recheckPermit(ctx context.Context) *Error {
	if h, _ := ctx.Value(permitKey{}).(*permit); h != nil && h.recheck != nil {
		if skip := h.recheck(); skip != nil {
			skip.terminal = true
			return skip
		}
	}
	return nil
}

func (s *runState) attempt(ctx, actx context.Context, r Engine, sink Sink, notify func(string)) *Error {
	// Every attempt resumes after a possible idle: another task may have marked the account dead.
	if skip := recheckPermit(ctx); skip != nil {
		return s.note(skip)
	}
	actx, gerr := guardAttempt(actx, r, s.req)
	if gerr != nil {
		return s.note(gerr)
	}
	cw := &runSink{Sink: sink, agent: r.Name()}
	out, err := r.Run(actx, s.req, cw)
	// Preserve the actual agent named by a delegating engine.
	out.Agent, out.Dropped, out.Emulated = cmp.Or(out.Agent, s.res.Agent), append(slices.Clone(s.dropped), out.Dropped...), s.emulated
	s.res = out
	e := Classify(r.Name(), err, cw.progressed)
	fallToNextAlias(r, &s.req, e, notify)
	if e != nil {
		_ = s.note(e)
	}
	return e
}

func (s *runState) finish(name string, start time.Time, e *Error, recoverErr error) (Result, error) {
	res := s.res
	res.DurationMs = time.Since(start).Milliseconds()
	res.Status = statusOf(e)
	if res.Transport == nil {
		res.Transport = &Transport{}
	}
	res.Transport.Attempts = s.attempts
	if e == nil {
		res.recovered = s.failed
		if s.req.OutputSchema != "" { // Every answer is checked, native or emulated.
			e = checkStructured(&res, s.req.OutputSchema, name)
		}
	}
	if e != nil {
		e.Agent = cmp.Or(e.Agent, name)
		condemnAuth(e, recoverErr)
		return res, e
	}
	return res, nil
}

// runPolicy is DefaultPolicy under req's timeout, ctx's cooldown key and ctx's permit.
func runPolicy(ctx context.Context, req RunOptions, notify func(string)) Policy {
	p := DefaultPolicy()
	if req.Timeout > 0 {
		p.AttemptTimeout = req.Timeout
	}
	p.Notify = notify
	p.Key, _ = ctx.Value(coolKey{}).(string)
	if h, _ := ctx.Value(permitKey{}).(*permit); h != nil {
		p.Idle = h.idle
	}
	return p
}
