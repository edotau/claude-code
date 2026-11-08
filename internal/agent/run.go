package agent

import (
	"cmp"
	"context"
	"fmt"
	"github.com/edotau/claude-code/internal/agent/retry"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/answer"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

type coolKey struct{}

// withCoolKey names the provider account a Run under ctx shares its rate limit with.
func withCoolKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, coolKey{}, key)
}

// fallToNextAlias: a hard-quota 429 means this key can never use that model, so try the next alias, not a wait.
func fallToNextAlias(r Engine, req *RunOptions, e *retry.Error, notify func(string)) {
	if e != nil && (e.Kind == retry.KindRateLimit || e.Kind == retry.KindQuota) && providers.ModelDenied(e.Err.Error()) &&
		r.Caps().Model != None && (req.Model == "" || slices.Contains(models.AliasNames, req.Model)) {
		cur := cmp.Or(req.Model, models.Opus)
		if next := models.NextAlias(cur); next != "" {
			if notify != nil {
				notify(fmt.Sprintf("%s: the %s model is quota-dead on this key; retrying on %s", r.Name(), cur, next))
			}
			req.Model, e.RetryAfter, e.Kind = next, 0, retry.KindRateLimit
		}
	}
}

// checkStructured fills res's structured answer; a schema failure also sets res.Status.
func checkStructured(res *Result, schema, agentName string) *retry.Error {
	var verr error
	if res.Structured, res.SchemaUnchecked, verr = answer.Structured(cmp.Or(string(res.Structured), res.Answer), schema); verr != nil {
		e := &retry.Error{Kind: retry.KindFatal, Agent: agentName, Err: verr}
		res.Status = statusOf(e)
		return e
	}
	return nil
}

// resolveTarget applies --provider then --model (alias, id, or provider:model) to the registry; an id a Claude alias
// currently backs takes the session's alias selection, so CLAUDE_<TIER>_MODEL pins win.
func resolveTarget(req RunOptions) (*providers.Registry, providers.Target, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Target{}, err
	}
	req = profileAliasRequest(reg, req)
	sel, err := providers.Select(reg, cmp.Or(req.Provider, registeredSession(reg)))
	if sel.Provider == nil || (err != nil && req.Model == "") {
		return nil, providers.Target{}, err
	}
	if req.Model == "" {
		return reg, sel.Aliases[models.Opus], nil
	}
	if t, ok := sel.Aliases[req.Model]; ok {
		return reg, t, nil
	}
	if t, ok := aliasTarget(sel, req.Model); ok && !namesProvider(reg, req.Model) {
		return reg, t, nil
	}
	return reg, providers.ParseTarget(reg, req.Model, sel.Provider), nil
}

// "<profile>:<alias>" resolves like --provider <profile> --model <alias>.
func profileAliasRequest(reg *providers.Registry, req RunOptions) RunOptions {
	if name, alias, ok := strings.Cut(req.Model, ":"); ok && slices.Contains(models.AliasNames, alias) && reg.Providers[name] != nil {
		req.Provider, req.Model = name, alias
	}
	return req
}

// aliasTarget is the session's target for a canonical Claude id that an alias backs (any spelling), [1m] kept.
func aliasTarget(sel providers.Selection, model string) (providers.Target, bool) {
	canonical, oneM, ok := models.Resolve(model)
	if !ok {
		return providers.Target{}, false
	}
	t, ok := sel.Aliases[models.AliasOf(canonical)]
	if ok && oneM {
		t.OneM = true
	}
	return t, ok
}

// registeredSession is the session's profile when the registry has it, else "": it beats the vault pin.
func registeredSession(reg *providers.Registry) string {
	if name := providers.SessionProfile(); reg != nil && reg.Providers[name] != nil {
		return name
	}
	return ""
}

// Run drops unsupported fields loudly, then runs r under DefaultPolicy with req.Timeout per attempt.
func Run(ctx context.Context, r Engine, req RunOptions, sink Sink, notify func(string)) (Result, error) {
	if sink == nil {
		sink = Discard
	}
	req, err := checkRun(r, req)
	if err != nil {
		return Result{Agent: r.Name(), Status: StatusError}, &retry.Error{Kind: retry.KindFatal, Agent: r.Name(), Err: err}
	}
	req, dropped, emulated := applyCaps(r.Caps(), req)
	ctx = withPermissionsEnv(ctx, req.Permissions)
	res := Result{Agent: r.Name(), Status: StatusError, Dropped: dropped, Emulated: emulated}
	// a no-tools one-shot answers from the provider's API, so it needs no agent binary up front
	if !req.NoTools {
		if err := r.Available(); err != nil {
			return res, &retry.Error{Kind: retry.KindFatal, Agent: r.Name(), Err: err}
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
	e := retry.Do(ctx, p, func(actx context.Context, attempt int) *retry.Error {
		state.attempts = attempt
		return state.attempt(ctx, actx, r, sink, notify)
	})
	return state.finish(r.Name(), start, e, recoverErr)
}

// checkRun resolves inherited permissions, then refuses a persona, read-only or permission set r cannot honor.
func checkRun(r Engine, req RunOptions) (RunOptions, error) {
	var err error
	if req.Permissions, err = inheritPermissions(req.Permissions); err != nil {
		return req, err
	}
	if err = RefusePersona(r.Caps(), r.Name(), req.Persona); err != nil {
		return req, err
	}
	if err = RefuseReadOnly(r.Caps(), r.Name(), req.ReadOnly); err != nil {
		return req, err
	}
	return req, RefusePermissions(r.Caps(), r.Name(), req.Permissions)
}

type runState struct {
	res               Result
	req               RunOptions
	dropped, emulated []string
	attempts          int
	failed            []*retry.Error
}

func (s *runState) note(e *retry.Error) *retry.Error {
	s.failed = append(s.failed, retry.WithoutEarlier(e))
	return e
}

func recheckPermit(ctx context.Context) *retry.Error {
	if h := retry.Held(ctx); h != nil && h.Recheck != nil {
		if skip := h.Recheck(); skip != nil {
			skip.Terminal = true
			return skip
		}
	}
	return nil
}

func (s *runState) attempt(ctx, actx context.Context, r Engine, sink Sink, notify func(string)) *retry.Error {
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
	e := retry.Classify(r.Name(), err, cw.progressed)
	fallToNextAlias(r, &s.req, e, notify)
	if e != nil {
		_ = s.note(e)
	}
	return e
}

func (s *runState) finish(name string, start time.Time, e *retry.Error, recoverErr error) (Result, error) {
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
func runPolicy(ctx context.Context, req RunOptions, notify func(string)) retry.Policy {
	p := retry.DefaultPolicy()
	if req.Timeout > 0 {
		p.AttemptTimeout = req.Timeout
	}
	p.Notify = notify
	p.Key, _ = ctx.Value(coolKey{}).(string)
	if h := retry.Held(ctx); h != nil {
		p.Idle = h.Idle
	}
	return p
}
