// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot)
// under one retry loop; every agent returns the same Result envelope.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Client assembles and executes recorded agent dispatches.
type Client struct {
	Launcher Launcher
	Alias    func(task string) string
	Refresh  func(ctx context.Context, account string) bool
	Notify   func(string)
}

// NewClient accepts classification and refresh seams without depending on their CLI implementations.
func NewClient(alias func(string) string, refresh func(context.Context, string) bool, notify func(string)) *Client {
	return &Client{Launcher: defaultLauncher, Alias: alias, Refresh: refresh, Notify: notify}
}

// Prepared is a routed, recorded dispatch that has not started.
type Prepared struct {
	Opts   RunOptions
	Record Record
	Chain  Chain
	remote Engine
	pack   *packFile // the pack an agent reads from the workdir; written by Exec, removed after the run
}

// Prepare routes the raw task before applying its rubric and claiming a dispatch record.
func (c *Client) Prepare(ctx context.Context, o RunOptions, claim *Record) (*Prepared, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return nil, err
	}
	if err := refuseDirect(o.Direct); err != nil {
		return nil, clientUsageError{err}
	}
	r, err := Lookup(o.Agent)
	if err != nil {
		return nil, err
	}
	p := &Prepared{}
	if o.Host != "" {
		if r, err = hostEngine(o); err != nil {
			return nil, err
		}
		p.remote = r
	}
	if p.Chain, err = c.route(ctx, r, o); err != nil {
		return nil, err
	}
	if err := refusePersona(r.Caps(), r.Name(), o.Persona); err != nil {
		return nil, clientUsageError{err}
	}
	if o, err = withRubric(o); err != nil {
		return nil, err
	}
	if err := c.finishPrepare(ctx, p, o, claim); err != nil {
		return nil, err
	}
	return p, nil
}

// finishPrepare sets p's options and dispatch record, then packs o's workspace context into the prompt.
func (c *Client) finishPrepare(ctx context.Context, p *Prepared, o RunOptions, claim *Record) error {
	if o.Pack != nil && p.remote != nil {
		return clientUsageError{errors.New("--dirs/--files/--diff: not with --host")}
	}
	p.Opts = o
	p.Record = c.record(ctx, o, claim)
	p.Record.Preflight = p.Chain.Preflight
	if o.Pack == nil {
		return nil
	}
	if err := c.packPrompt(ctx, p); err != nil { // the record is already claimed on disk: close it
		p.Record.finishSave(wire.Result{Agent: o.Agent, Status: wire.StatusError}, err, time.Since(p.Record.Time), c.Notify)
		return err
	}
	return nil
}

// hostEngine is the remote engine for o.Host.
func hostEngine(o RunOptions) (Engine, error) {
	if err := textutil.ValidHost(o.Host); err != nil {
		return nil, err
	}
	return newRemoteEngine(o.Agent, o.Host, remote.Bin(o.RemoteBin), o.Fallback), nil
}

// withRubric is o with its prompt wrapped in o.Rubric, when one is set.
func withRubric(o RunOptions) (RunOptions, error) {
	if o.Rubric == "" {
		return o, nil
	}
	var err error
	o.Prompt, err = rubricPrompt(o.Rubric, o.Prompt)
	return o, err
}

// withDefaults fills the agent, fallback and launcher defaults and resolves o.Agent to its agent.
func (c *Client) withDefaults(o RunOptions) (RunOptions, error) {
	o.Agent, o.Fallback, o.Launcher = cmp.Or(o.Agent, "claude"), cmp.Or(o.Fallback, router.DefaultFallback()), c.Launcher
	if o.SessionID != "" {
		o.Fallback = router.FallbackNone
	}
	return o, c.resolve(&o)
}

// Record builds the dispatch record for o without routing, persona refusal or rubric (live sessions, review jobs).
func (c *Client) Record(ctx context.Context, o RunOptions, claim *Record) (Record, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return Record{}, err
	}
	return c.record(ctx, o, claim), nil
}

// resolve maps o.Agent — a roster name, an agents key, vendor:name or a definition file — to its agent and
// applies the definition's model, profile and effort under the caller's explicit options.
func (c *Client) resolve(o *RunOptions) error {
	if _, ok := LookupSchema(o.Agent); ok && len(o.Agents) == 0 { // a plain roster agent, not a definition selector
		return nil
	}
	name, err := SelectAndOverlay(o.Agent, o.Agents, o)
	if err != nil {
		return clientUsageError{err}
	}
	o.Agent = name
	return nil // an explicit persona (a --retry record) survives a plain roster name
}

// SelectAndOverlay resolves ref to its agent and, when it names a definition, records the persona and
// fills the unset model, profile and effort under the options already in o.
func SelectAndOverlay(ref string, agents map[string]*Schema, o *RunOptions) (name string, err error) {
	name, def, err := SelectAgent(ref, agents)
	if err != nil {
		return "", err
	}
	if def != nil {
		o.Persona = def
		o.Model, o.Provider, o.Effort = cmp.Or(o.Model, defModel(def)), cmp.Or(o.Provider, def.Profile), cmp.Or(o.Effort, def.Effort)
	}
	return name, nil
}

// defModel is the definition's model for routing; "inherit" means no model of its own.
func defModel(def *Schema) string {
	if def.Model == "inherit" {
		return ""
	}
	return def.Model
}

type clientUsageError struct{ error }

func (clientUsageError) ExitCode() int { return 2 }

// route is Resolve on o: o.Provider is a pinned backend, o.Lead a resolved lead, --host one remote dispatch.
func (c *Client) route(ctx context.Context, r Engine, o RunOptions) (Chain, error) {
	chain, _, err := Resolve(ctx, RouteReq{Agent: o.Agent, Model: o.Model, AliasHint: c.alias(o.Prompt),
		Profile: o.Provider, Lead: o.Lead, Fallback: o.Fallback, WorkDir: o.WorkDir, Native: o.Native, Pinned: true,
		Host: o.Host != "", Engine: r, Refresh: c.Refresh})
	return chain, err
}

func (c *Client) alias(prompt string) string {
	if c.Alias != nil {
		return c.Alias(prompt)
	}
	return ""
}

func (c *Client) record(ctx context.Context, o RunOptions, claim *Record) Record {
	dir := o.WorkDir
	if o.Host != "" {
		dir = ""
	}
	rec := buildAskRecord(ctx, o.Agent, o, claim, dir, o.Background)
	rec.Request.Launcher = c.Launcher
	if claim == nil {
		rec.FallbackPolicy = o.Fallback
		if o.Host != "" {
			rec.Host, rec.RemoteBin = o.Host, remote.Bin(o.RemoteBin)
		}
	}
	return rec
}

// Exec runs a prepared dispatch, leaving initial record saving and background spawning to the caller.
func (c *Client) Exec(ctx context.Context, p *Prepared, out wire.Sink, d RunRecordedOpts) (wire.Result, error) {
	if out == nil {
		out = wire.Discard
	}
	if d.Notify == nil {
		d.Notify = c.Notify
	}
	ctx = withUnsetEnv(ctx, p.Opts.Unset)
	if p.pack != nil {
		if err := p.pack.write(); err != nil {
			res := wire.Result{Agent: p.Chain.Want, Status: wire.StatusError, DispatchID: p.Record.ID}
			p.Record.finishSave(res, err, time.Since(p.Record.Time), d.Notify)
			return res, err
		}
		defer p.pack.remove()
	}
	res, err := runRecorded(ctx, &p.Record, p.Chain.Want, out, d, func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		if p.remote != nil {
			return p.remote.Run(ctx, p.Opts, sink)
		}
		return runWithFallback(ctx, p.Chain, p.Opts, sink, d.Notify)
	})
	res.DispatchID = p.Record.ID
	return res, err
}

// Run prepares, saves and executes one dispatch; a nil output discards the caller's stream.
func (c *Client) Run(ctx context.Context, o RunOptions, out wire.Sink) (wire.Result, error) {
	p, err := c.Prepare(ctx, o, nil)
	if err != nil {
		return wire.Result{Agent: cmp.Or(o.Agent, "claude"), Status: wire.StatusError}, err
	}
	if err := p.Record.Save(); err != nil && c.Notify != nil { // fail-open: the log never gates the run
		c.Notify(fmt.Sprintf("dispatch %s record not saved before the run: %v", p.Record.ID, err))
	}
	return c.Exec(ctx, p, out, RunRecordedOpts{})
}

// Stream runs a dispatch and returns its buffered event stream.
func (c *Client) Stream(ctx context.Context, o RunOptions) <-chan wire.Event {
	return c.streamWith(ctx, cmp.Or(o.Agent, "claude"), func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		return c.Run(ctx, o, sink)
	})
}

func (c *Client) streamWith(
	ctx context.Context, agentName string, fn func(context.Context, wire.Sink) (wire.Result, error),
) <-chan wire.Event {
	ctx, cancel := context.WithCancelCause(ctx)
	sink := newChanSink(cancel)
	go func() {
		defer close(sink.ch)
		defer cancel(nil)
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
			sink.mu.Lock()
			sawResult := sink.sawResult
			sink.mu.Unlock()
			if err != nil && !sawResult {
				_ = sink.Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
			}
			sink.mu.Lock()
			dropped := sink.dropped
			sink.mu.Unlock()
			if dropped > 0 && c.Notify != nil {
				c.Notify(fmt.Sprintf("agent: stream dropped %d events", dropped))
			}
		}()
		_, err = fn(ctx, sink)
	}()
	return sink.ch
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
	_, t, err := ResolveRequestTarget(req)
	return t, err == nil && t.Provider != nil
}

// checkRun layers r's default permissions under the request and the outer dispatch's over it, then refuses a persona or
// permission set r cannot honor; an emulated read-only becomes deny rules.
func checkRun(r Engine, req RunOptions) (RunOptions, error) {
	def, err := DefaultPermissions(r.Name())
	if err != nil {
		return req, err
	}
	req.Permissions, _ = resolvePermissions(def, req.Permissions)
	if req.Permissions, err = inheritPermissions(req.Permissions); err != nil {
		return req, err
	}
	if err = refusePersona(r.Caps(), r.Name(), req.Persona); err != nil {
		return req, err
	}
	if req.ReadOnly && r.Caps().ReadOnly == Emulated { // the agent enforces the read-only deny rules itself
		req.Permissions.Deny = append(slices.Clone(req.Permissions.Deny), ReadOnlyDeny...)
	}
	return req, refusePermissions(r.Caps(), r.Name(), req.Permissions)
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
	res.Status = StatusOf(e)
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
	Native                                              bool               `json:"native,omitempty"` // opencode: skip the hub ladder; --retry replays it
	Host                                                string             `json:"-"`
	RemoteBin                                           string             `json:"-"`
	Rubric                                              string             `json:"-"`
	Background                                          bool               `json:"-"`
}

// StatusOf maps the classified failure to the envelope status: quota, deadline, output-then-failure, else error.
func StatusOf(e *retry.Error) string {
	switch {
	case e == nil:
		return wire.StatusOK
	case e.Kind == retry.KindQuota:
		return wire.StatusQuota
	case errors.Is(e, context.DeadlineExceeded), errors.Is(e, ErrStalled), errors.Is(e, ErrSilent):
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
		out = append(out, engineFor(s.Name))
	}
	return out
}

// engineFor is the registered roster agent name as an Engine: its own, else a CLI engine over its headless driver.
func engineFor(name string) Engine {
	if e, ok := mustDriver(name).(Engine); ok {
		return e
	}
	return newCLIEngine(name)
}

// Lookup finds an agent by name.
func Lookup(name string) (Engine, error) {
	if _, ok := lookupRow(name); ok {
		return engineFor(name), nil
	}
	return nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(RosterNames(), "|"))
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

// refusePermissions fails closed when req carries Deny rules (or plan mode) and the agent cannot read Permissions.
func refusePermissions(c Caps, agentName string, p Permissions) error {
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
	if e := accountRefused(c.Dispatches[0]); e != nil && e.Kind == retry.KindQuota {
		return Chain{Want: r.Name(), Skipped: []string{r.Name() + ": " + e.Err.Error()}}
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
	return armClaudeProfile(leadProfile(out, disp), disp)
}

// leadProfile runs a hop that a non-Databricks confinement or the typed lead put on its account's row, never on the pin.
func leadProfile(out RunOptions, disp Dispatch) RunOptions {
	cp, _ := router.Confined()
	provider, profile, ok := strings.Cut(disp.Account, "·")
	if !ok || out.Provider != "" || (provider != cp && profile != out.Lead) {
		return out
	}
	if reg, err := providers.Load(); err == nil && reg.Providers[profile] != nil {
		out.Provider = profile
	}
	return out
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

type coolKey struct{}

// withCoolKey names the provider account a Run under ctx shares its rate limit with.
func withCoolKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, coolKey{}, key)
}

// fallToNextAlias: a hard-quota 429 means this key can never use that model, so try the next alias, not a wait.
func fallToNextAlias(r Engine, req *RunOptions, e *retry.Error, notify func(string)) {
	if !aliasDenied(r, *req, e) {
		return
	}
	cur := cmp.Or(req.Model, models.Opus)
	if next := models.NextAlias(cur); next != "" {
		if notify != nil {
			notify(fmt.Sprintf("%s: the %s model is quota-dead on this key; retrying on %s", r.Name(), cur, next))
		}
		req.Model, e.RetryAfter, e.Kind = next, 0, retry.KindRateLimit
	}
}

// aliasDenied reports e as a rate or quota refusal of req's alias model (none: opus) on an agent that takes --model.
func aliasDenied(r Engine, req RunOptions, e *retry.Error) bool {
	return e != nil && (e.Kind == retry.KindRateLimit || e.Kind == retry.KindQuota) && providers.ModelDenied(e.Err.Error()) &&
		r.Caps().Model != None && (req.Model == "" || slices.Contains(models.AliasNames, req.Model))
}

// checkStructured fills res's structured answer; a schema failure also sets res.Status.
func checkStructured(res *wire.Result, schema, agentName string) *retry.Error {
	var verr error
	if res.Structured, res.SchemaUnchecked, verr = wire.Structured(cmp.Or(string(res.Structured), res.Answer), schema); verr != nil {
		e := &retry.Error{Kind: retry.KindFatal, Agent: agentName, Err: verr}
		res.Status = StatusOf(e)
		return e
	}
	return nil
}

// ResolveRequestTarget applies --provider then --model (alias, id, or provider:model) to the registry; an id a Claude alias
// currently backs takes the session's alias selection, so CLAUDE_<TIER>_MODEL pins win.
func ResolveRequestTarget(req RunOptions) (*providers.Registry, providers.Target, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Target{}, err
	}
	return resolveIn(reg, req)
}

// resolveIn is ResolveRequestTarget against an already loaded registry.
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
	return reg, servedTarget(cmp.Or(req.Agent, "claude"), providers.ParseTarget(reg, req.Model, sel.Provider)), nil
}

// servedTarget is t with its model in preflight's spelling: a bare id the workspace serves runs as databricks-<id>.
func servedTarget(agentName string, t providers.Target) providers.Target {
	if t.Provider != nil && t.Provider.Preset == providers.PresetDatabricks {
		t.Model = launchWire(agentName, t.Provider, t.Model)
	}
	return t
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
	if ok && t.Model == "auto" { // a router alias names no model: the typed id stays exact
		return providers.Target{}, false
	}
	if ok && oneM {
		t.OneM = true
	}
	return t, ok
}

const streamBuffer = 1024

var streamStall = 2 * time.Minute

// errStreamAbandoned is the cancellation cause when a stream stays full beyond streamStall.
var errStreamAbandoned = errors.New("agent: stream consumer abandoned")

type chanSink struct {
	wire.Stamper
	mu        sync.Mutex
	ch        chan wire.Event
	cancel    context.CancelCauseFunc
	fullSince time.Time
	dropped   int
	sawResult bool
}

func newChanSink(cancel context.CancelCauseFunc) *chanSink {
	return &chanSink{Stamper: wire.NewStamper(), ch: make(chan wire.Event, streamBuffer), cancel: cancel}
}

// Emit stamps ev and drops the oldest buffered event when the consumer falls behind.
func (s *chanSink) Emit(ev wire.Event) error {
	if ev.Type == wire.EventProgress { // a retry-gate mark for the relay, not an event a Stream consumer reads
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Stamp(&ev)
	if ev.Type == wire.EventResult {
		s.sawResult = true
	}
	select {
	case s.ch <- ev:
		s.fullSince = time.Time{}
		return nil
	default:
	}
	s.replaceOldest(ev)
	return nil
}

func (s *chanSink) replaceOldest(ev wire.Event) {
	if s.fullSince.IsZero() {
		s.fullSince = wire.Clock()
	} else if wire.Clock().Sub(s.fullSince) > streamStall {
		s.cancel(errStreamAbandoned)
	}
	select {
	case <-s.ch:
		s.dropped++
	default:
	}
	select {
	case s.ch <- ev:
	default:
	}
}

// packReserveTokens is the output headroom in the prompt-cap rule: window − 32k − 10%.
const packReserveTokens = 32_000

// packFile is a pack the agent reads from the workdir instead of the prompt (CLI.PackVia == PackFile).
type packFile struct{ path, body string }

func (f *packFile) write() error {
	out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("pack file: %w", err)
	}
	_, err = out.WriteString(f.body)
	if err = errors.Join(err, out.Close()); err != nil {
		return fmt.Errorf("pack file: %w", errors.Join(err, os.Remove(f.path)))
	}
	return nil
}

func (f *packFile) remove() { _ = os.Remove(f.path) }

// packBudget is the byte budget for d: its model window under the prompt-cap rule, then its transport cap
// unless the pack travels as a file (the prompt then carries only the instruction).
func packBudget(d Dispatch, req RunOptions, viaFile bool) (budget int, note string) {
	s := SchemaFor(d.Agent.Name())
	model := d.Model
	if model == "" {
		if t, _, err := s.Target(req); err == nil {
			model = t.Model
		}
	}
	if model == "" {
		note = d.Agent.Name() + ": model unknown; packing to the 200K-token default window"
	}
	budget = pack.BudgetBytes(models.PromptTokens(models.Window(model), packReserveTokens))
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && n < budget && !viaFile {
		budget, note = n, d.Agent.Name()+": "+s.CLI.PromptVia.String()+" prompt cap"
	}
	return budget, note
}

// packPrompt packs p.Opts.Pack around the task for the lead dispatch, drops later dispatches the pack would
// not fit, and either inlines the pack in the prompt or, for a PackFile lead, stages it as a file to read.
func (c *Client) packPrompt(ctx context.Context, p *Prepared) error {
	o := p.Opts
	if len(p.Chain.Dispatches) == 0 {
		return nil // routing already recorded why nothing can run
	}
	cwd, err := filepath.Abs(o.WorkDir) // "" is the process cwd; the child starts inside it, so the prompt path must be absolute
	if err != nil {
		return clientUsageError{err}
	}
	lead := p.Chain.Dispatches[0]
	viaFile := SchemaFor(lead.Agent.Name()).CLI.PackVia == PackFile
	budget, note := packBudget(lead, o, viaFile)
	packed, err := pack.Build(ctx, *o.Pack, o.Prompt, cwd, budget)
	if err != nil {
		return clientUsageError{err}
	}
	notes := append(packed.Notes, note)
	if viaFile {
		p.pack = &packFile{path: filepath.Join(cwd, pack.FilePrefix+p.Record.ID+".md"), body: packed.Prompt}
		p.Opts.Prompt = o.Prompt + "\n\nThe workspace context for this task is in " + p.pack.path +
			". Read that file in full with your file tools before answering."
		p.Opts.Tools, p.Record.Request.Tools = true, true
		notes = append(notes, fmt.Sprintf("%s: pack (%d KB) is read from a file; tools are on for this run (read-only intent)",
			lead.Agent.Name(), len(packed.Prompt)/1024))
	} else {
		p.Opts.Prompt = packed.Prompt
	}
	for _, n := range notes {
		if n != "" && c.Notify != nil {
			c.Notify(n)
		}
	}
	p.Chain = fitChain(p.Chain, len(packed.Prompt), o, viaFile)
	return nil
}

// fitChain keeps the lead and every later dispatch whose budget holds n bytes (no transport cap when viaFile).
func fitChain(c Chain, n int, o RunOptions, viaFile bool) Chain {
	out := Chain{Want: c.Want, Dispatches: c.Dispatches[:1:1], Skipped: slices.Clone(c.Skipped)}
	for _, d := range c.Dispatches[1:] {
		if b, _ := packBudget(d, o, viaFile); b < n {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: packed prompt (%d KB) is over its %d KB budget", d.Agent.Name(), n/1024, b/1024))
			continue
		}
		out.Dispatches = append(out.Dispatches, d)
	}
	return out
}

// FormatStreamJSON is the --format value for the newline-delimited event stream.
const FormatStreamJSON = "stream-json"

// Formats is every allowed --format value.
var Formats = []string{helptext.Text, helptext.JSON, FormatStreamJSON}

// ValidateAs validates --as; it applies to claude only, other agents follow the profile's identity setting.
func ValidateAs(as, ag string) error {
	if as == "" {
		return nil
	}
	if as != providers.IdentityAuto && as != providers.IdentityUser && as != providers.IdentitySP {
		return fmt.Errorf("--as %q must be auto, user or sp", as)
	}
	if ag != "claude" {
		return fmt.Errorf("--as applies to claude only; %s follows the profile's identity setting", ag)
	}
	return nil
}

// buildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func buildAskRecord(ctx context.Context, name string, req RunOptions, jobRec *Record, workdir string, background bool) Record {
	if jobRec != nil {
		return *jobRec // same id: a job run continues the record it was spawned from
	}
	rec := ClaimRecord(name, req, time.Now())
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rec.Repo = RepoRoot(ctx, cwd)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec
}
