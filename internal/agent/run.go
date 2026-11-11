// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot)
// under one retry loop; every agent returns the same Result envelope.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
func checkStructured(res *wire.Result, schema, agentName string) *retry.Error {
	var verr error
	if res.Structured, res.SchemaUnchecked, verr = wire.Structured(cmp.Or(string(res.Structured), res.Answer), schema); verr != nil {
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
	return resolveIn(reg, req)
}

// resolveIn is resolveTarget against an already loaded registry.
func resolveIn(reg *providers.Registry, req RunOptions) (*providers.Registry, providers.Target, error) {
	req = profileAliasRequest(reg, req)
	sel, err := providers.Select(reg, cmp.Or(req.Provider, providers.SessionLead(reg, false)))
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

// Run drops unsupported fields loudly, then runs r under DefaultPolicy with req.Timeout per attempt.
func Run(ctx context.Context, r Engine, req RunOptions, sink wire.Sink, notify func(string)) (wire.Result, error) {
	if sink == nil {
		sink = wire.Discard
	}
	req, err := checkRun(r, req)
	if err != nil {
		return wire.Result{Agent: r.Name(), Status: wire.StatusError}, retry.Fatal(r.Name(), err)
	}
	req, dropped, emulated := applyCaps(r.Caps(), req)
	ctx = withPermissionsEnv(ctx, req.Permissions)
	res := wire.Result{Agent: r.Name(), Status: wire.StatusError, Dropped: dropped, Emulated: emulated}
	// a no-tools one-shot answers from the provider's API, so it needs no agent binary up front
	if !req.NoTools {
		if err := r.Available(); err != nil {
			return res, retry.Fatal(r.Name(), err)
		}
	}
	p := runPolicy(ctx, req, notify)
	var recoverErr error
	if t, ok := recoveryTarget(r, req); ok && (t.Provider.Auth.Command != "" || t.Provider.Auth.Sealed()) {
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

// recoveryTarget is the provider a run bills: an external CLI's own pick (pin, session, gate), else the flags'.
func recoveryTarget(r Engine, req RunOptions) (providers.Target, bool) {
	if s, ok := LookupSchema(r.Name()); ok && s.External() {
		t, own, _, err := s.targetNote(req)
		return t, err == nil && !own && t.Provider != nil
	}
	_, t, err := resolveTarget(req)
	return t, err == nil && t.Provider != nil
}

// checkRun layers r's default permissions under the request and the outer dispatch's over it, then refuses a persona or
// permission set r cannot honor; an emulated read-only becomes deny rules.
func checkRun(r Engine, req RunOptions) (RunOptions, error) {
	def, err := DefaultPermissions(r.Name())
	if err != nil {
		return req, err
	}
	req.Permissions, _ = ResolvePermissions(def, req.Permissions)
	if req.Permissions, err = inheritPermissions(req.Permissions); err != nil {
		return req, err
	}
	if err = RefusePersona(r.Caps(), r.Name(), req.Persona); err != nil {
		return req, err
	}
	if req.ReadOnly && r.Caps().ReadOnly == Emulated { // the agent enforces the read-only deny rules itself
		req.Permissions.Deny = append(slices.Clone(req.Permissions.Deny), ReadOnlyDeny...)
	}
	return req, RefusePermissions(r.Caps(), r.Name(), req.Permissions)
}

type runState struct {
	res               wire.Result
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

func (s *runState) attempt(ctx, actx context.Context, r Engine, sink wire.Sink, notify func(string)) *retry.Error {
	// Every attempt resumes after a possible idle: another task may have marked the account dead.
	if skip := recheckPermit(ctx); skip != nil {
		return s.note(skip)
	}
	actx, gerr := guardAttempt(actx, r, s.req)
	if gerr != nil {
		return s.note(gerr)
	}
	cw := &wire.RunSink{Sink: sink, Agent: r.Name()}
	out, err := r.Run(actx, s.req, cw)
	// Preserve the actual agent named by a delegating engine.
	out.Agent, out.Dropped, out.Emulated = cmp.Or(out.Agent, s.res.Agent), append(slices.Clone(s.dropped), out.Dropped...), append(slices.Clone(s.emulated), out.Emulated...)
	s.res = out
	e := retry.Classify(r.Name(), err, cw.Progressed)
	fallToNextAlias(r, &s.req, e, notify)
	if e != nil {
		_ = s.note(e)
	}
	return e
}

func (s *runState) finish(name string, start time.Time, e *retry.Error, recoverErr error) (wire.Result, error) {
	res := s.res
	res.DurationMs = time.Since(start).Milliseconds()
	res.Status = statusOf(e)
	if res.Transport == nil {
		res.Transport = &wire.Transport{}
	}
	res.Transport.Attempts = s.attempts
	if e == nil {
		res.Recovered = s.failed
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
	switch {
	case req.Timeout > 0:
		p.AttemptTimeout = req.Timeout
	case req.Tools:
		p.AttemptTimeout = DefaultToolsTimeout
	}
	p.Notify = notify
	p.Key, _ = ctx.Value(coolKey{}).(string)
	if h := retry.Held(ctx); h != nil {
		p.Idle = h.Idle
	}
	return p
}

// DefaultToolsTimeout is the per-attempt deadline of a tools run without an explicit timeout.
const DefaultToolsTimeout = 45 * time.Minute

// RunOptions is one task. Model may be a model alias, a model id, or provider:model.
type RunOptions struct {
	Prompt, Model, Provider, Effort, WorkDir, SessionID string
	Lead                                                string      `json:",omitempty"` // lead profile for the pick; "" = its pin leads
	Attach                                              []string    // absolute file paths, task text precedes them
	KeepEnv                                             []string    // names the credential scrub must not drop, from --keep-env
	Tools, Autonomous, EnableMemory, Direct             bool        // Direct: agent keeps the provider route, skips the router
	Fleet                                               bool        // copilot only: run in fleet mode (parallel subagents)
	NoTools                                             bool        // claude only: answer from the provider directly, no claude process
	ReadOnly                                            bool        // tools may read but never write, run shell or commit
	NoPersist                                           bool        // claude, codex: no session persistence
	SafeMode                                            bool        // claude, codex, opencode: no hooks/plugins/customizations
	DebugFile                                           string      // claude, gemini: debug log at <path>
	SystemPrompt, AppendSystemPrompt                    string      // replace / extend the system prompt; native on claude, codex, copilot, opencode, emulated elsewhere
	Env                                                 []string    `json:"-"`                   // KEY=VALUE for the child, layered last; never logged
	EnvNames                                            []string    `json:"env_names,omitempty"` // the names of Env, all the dispatch log keeps
	Unset                                               []string    `json:"-"`                   // names dropped from the child's inherited env (an overlay's unset); never logged
	OverlaySet                                          []string    `json:"-"`                   // names an overlay file sets; refused where the route sets them
	OutputSchema                                        string      // JSON Schema text the answer must satisfy
	Permissions                                         Permissions `json:"permissions,omitzero"` // allow/ask/deny rules and mode; zero value = no policy
	Identity                                            string      `json:"identity,omitempty"`   // --as user|sp, claude only: the identity a live holder applies
	Persona                                             *Schema     `json:"persona,omitempty"`    // claude: native agent definition; others: emulated
	Pack                                                *pack.Spec  `json:"pack,omitempty"`       // inline workspace files into Prompt at Prepare; the record keeps the spec, not the bytes
	Timeout                                             time.Duration
	Launcher                                            Launcher           `json:"-"` // claude only: resolves the launch plan
	Agent                                               string             `json:"-"`
	Agents                                              map[string]*Schema `json:"-"`
	Fallback                                            string             `json:"-"`
	Native                                              bool               `json:"-"`
	Host                                                string             `json:"-"`
	RemoteBin                                           string             `json:"-"`
	Rubric                                              string             `json:"-"`
	Background                                          bool               `json:"-"`
}

// statusOf maps the classified failure to the envelope status: quota, deadline, output-then-failure, else error.
func statusOf(e *retry.Error) string {
	switch {
	case e == nil:
		return wire.StatusOK
	case e.Kind == retry.KindQuota:
		return wire.StatusQuota
	case errors.Is(e, context.DeadlineExceeded), errors.Is(e, ErrStalled):
		return wire.StatusStalled
	case e.Progressed:
		return wire.StatusPartial
	}
	return wire.StatusError
}

// Caps declares which RunOptions fields an agent honors; Prompt, Timeout and Attach are always honored.
type Caps struct {
	Model, Provider, Effort, WorkDir, SessionID, Tools, NoTools, Autonomous, Fleet, Memory, NoPersist, SafeMode, DebugFile, Direct Level
	ReadOnly                                                                                                                       Level
	SystemPrompt, AppendSystemPrompt, Env, OutputSchema, Persona                                                                   Level
	Permissions                                                                                                                    Level // reads RunOptions.Permissions
}

// Level is how an agent honors one flag: its own flag (Native), a harness shim (Emulated), or not at all (None).
type Level uint8

const (
	None Level = iota
	Emulated
	Native
)

func (l Level) String() string { return [...]string{"none", "emulated", "native"}[min(l, Native)] }

// MarshalText renders the level as its name, so the roster JSON prints "native" rather than 2.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Engine is one agent backend.
type Engine interface {
	Name() string
	Caps() Caps
	Available() error
	Run(ctx context.Context, req RunOptions, sink wire.Sink) (wire.Result, error)
}

// Engines lists every agent in roster order.
func Engines() []Engine {
	var out []Engine
	for _, s := range rows() {
		if e, ok := mustDriver(s.Name).(Engine); ok {
			out = append(out, e)
		} else {
			out = append(out, newCLIEngine(s.Name))
		}
	}
	return out
}

// Lookup finds an agent by name.
func Lookup(name string) (Engine, error) {
	var names []string
	for _, r := range Engines() {
		if r.Name() == name {
			return r, nil
		}
		names = append(names, r.Name())
	}
	return nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(names, "|"))
}

// applyCaps clears fields the agent cannot honor and names each drop; Emulated flags run a harness shim here.
func applyCaps(c Caps, req RunOptions) (out RunOptions, dropped, emulated []string) {
	drop := func(l Level, set bool, flag string, clear func()) {
		if set && l == None {
			dropped = append(dropped, flag)
			clear()
		}
	}
	drop(c.Model, req.Model != "", "--model="+req.Model, func() { req.Model = "" })
	drop(c.Provider, req.Provider != "", "--provider="+req.Provider, func() { req.Provider = "" })
	drop(c.Effort, req.Effort != "", "--effort="+req.Effort, func() { req.Effort = "" })
	drop(c.WorkDir, req.WorkDir != "", "--workdir="+req.WorkDir, func() { req.WorkDir = "" })
	drop(c.SessionID, req.SessionID != "", "--session="+req.SessionID, func() { req.SessionID = "" })
	drop(c.ReadOnly, req.ReadOnly, "--read-only (tools off)", func() { req.ReadOnly, req.Tools = false, false }) // fail closed
	drop(c.Tools, req.Tools, "--tools", func() { req.Tools = false })
	drop(c.NoTools, req.NoTools, "--no-tools", func() { req.NoTools = false })
	drop(c.Autonomous, req.Autonomous, "--autonomous", func() { req.Autonomous = false })
	drop(c.Fleet, req.Fleet, "--fleet", func() { req.Fleet = false })
	drop(c.Memory, req.EnableMemory, "--enable-memory", func() { req.EnableMemory = false })
	drop(c.NoPersist, req.NoPersist, "--no-persist", func() { req.NoPersist = false })
	drop(c.SafeMode, req.SafeMode, "--safe-mode", func() { req.SafeMode = false })
	drop(c.Direct, req.Direct, "--direct", func() { req.Direct = false })
	drop(c.DebugFile, req.DebugFile != "", "--debug-file="+req.DebugFile, func() { req.DebugFile = "" })
	drop(c.Env, len(req.Env) > 0, "--env", func() { req.Env = nil })
	drop(c.Permissions, !req.Permissions.IsZero(), "--allow/--ask/--mode", func() { req.Permissions = Permissions{} })
	drop(c.Persona, req.Persona != nil, "persona="+personaName(req.Persona), func() { req.Persona = nil })
	if req.Persona != nil && c.Persona == Emulated {
		req, dropped = emulatePersona(c, req, dropped)
		emulated = append(emulated, "persona")
	}
	for _, sp := range []struct {
		field *string
		level Level
		flag  string
	}{{&req.SystemPrompt, c.SystemPrompt, "--system-prompt"}, {&req.AppendSystemPrompt, c.AppendSystemPrompt, "--append-system-prompt"}} {
		if *sp.field != "" && sp.level == Emulated {
			req.Prompt, *sp.field = Preamble(*sp.field)+req.Prompt, ""
			emulated = append(emulated, sp.flag)
		}
	}
	drop(c.SystemPrompt, req.SystemPrompt != "", "--system-prompt", func() { req.SystemPrompt = "" })
	drop(c.AppendSystemPrompt, req.AppendSystemPrompt != "", "--append-system-prompt", func() { req.AppendSystemPrompt = "" })
	if req.OutputSchema != "" && c.OutputSchema != Native { // the answer is validated either way; only the instruction differs
		req.Prompt += wire.SchemaInstruction(req.OutputSchema)
		emulated = append(emulated, "--output-schema")
	}
	return req, dropped, emulated
}

// ReadOnlyDeny is --read-only as deny rules for an agent whose CLI has no read-only mode (ReadOnly: Emulated).
var ReadOnlyDeny = []Rule{{Tool: "Bash"}, {Tool: "Write"}, {Tool: "Edit"}}

// RefusePermissions fails closed when req carries Deny rules (or plan mode) and the agent cannot read Permissions.
func RefusePermissions(c Caps, agentName string, p Permissions) error {
	if c.Permissions != None || (len(p.Deny) == 0 && p.Mode != "plan") {
		return nil
	}
	what := "permission deny rules"
	if len(p.Deny) == 0 {
		what = "--mode plan"
	}
	return errors.New(agentName + " cannot enforce " + what + " yet; dispatch refused")
}

// explicitChain is r alone on the caller's explicit backend (--provider or provider:model), carrying that backend's
// account: a remembered exhaustion skips it without an attempt, and a quota failure is recorded on it.
func explicitChain(r Engine, req RunOptions) Chain {
	c := Single(r)
	acct, billing := explicitAccount(r, req)
	c.Dispatches[0].Account, c.Dispatches[0].Billing = acct, billing
	if req.Model == router.DiscoveryModel {
		reg := loadRegistryWarn()
		c.Dispatches[0] = discoveryDispatch(c.Dispatches[0], router.Aliases[1], reg, req.Provider, false)
	}
	if acct == "" || acct == copilotLoginAccount() { // copilot's own gate falls back to Databricks instead
		return c
	}
	now := time.Now()
	if e := loadState()[acct]; e.Status(now) == router.StateExhausted {
		return Chain{Want: r.Name(), Skipped: []string{r.Name() + ": " + router.ExhaustedReason(acct, e, now)}}
	}
	return c
}

// withAlias runs c's lone explicit dispatch on the router's alias when the caller named no model.
func withAlias(c Chain, alias string) Chain {
	if len(c.Dispatches) != 1 || alias == "" {
		return c
	}
	disp := c.Dispatches[0]
	disp.Model, disp.keepCaller, disp.unroutedDispatch = alias, false, false
	c.Dispatches = []Dispatch{disp}
	return c
}

// dispatchRequest is req for dispatch: the wanted dispatch keeps an explicit caller model; every other dispatch drops the original
// provider and session (they belong to the dispatch that minted them) and runs its cell, the caller's --effort winning.
func dispatchRequest(req RunOptions, disp Dispatch, first bool) RunOptions {
	disp.discoveryNote()
	out := req
	if disp.unroutedDispatch {
		return out
	}
	if !first {
		out.Provider, out.SessionID = "", ""
	}
	if !disp.keepCaller {
		out.Model = disp.Model
	}
	if out.Effort == "" {
		out.Effort = disp.Effort
	}
	return armClaudeProfile(out, disp)
}

// armClaudeProfile runs a claude hop inside databricks:<p> on its "<profile>:" row; claude reads only Provider.
func armClaudeProfile(out RunOptions, disp Dispatch) RunOptions {
	if router.ArmProfile() == "" || disp.Agent == nil || disp.Agent.Name() != "claude" {
		return out
	}
	prof, model, ok := strings.Cut(out.Model, ":")
	if reg, err := providers.Load(); ok && err == nil && reg.Providers[prof] != nil {
		out.Provider, out.Model = prof, model
	}
	return out
}
