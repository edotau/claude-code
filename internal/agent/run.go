// Package agent runs one headless task on an agent (claude, codex, gemini, opencode, copilot) under one retry loop.
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
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/proc"
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
	ctx, o.Values = runValues(ctx, o.Values)
	o, err := c.withDefaults(o)
	if err != nil {
		return nil, err
	}
	if err := refuseDirectContext(ctx, o.Direct); err != nil {
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
	fallback := router.FallbackNone
	if strings.TrimSpace(proc.Value(valuesContext(context.Background(), o.Values), router.EnvFallback)) != "" ||
		strings.TrimSpace(providers.CustomEnv()[router.EnvFallback]) != "" {
		fallback = router.FallbackAll
	}
	o.Agent, o.Fallback, o.Launcher = cmp.Or(o.Agent, "claude"), cmp.Or(o.Fallback, fallback), c.Launcher
	if o.SessionID != "" {
		o.Fallback = router.FallbackNone
	}
	return o, c.resolve(&o)
}

// Record builds the dispatch record for o without routing, persona refusal or rubric (live sessions, review jobs).
func (c *Client) Record(ctx context.Context, o RunOptions, claim *Record) (Record, error) {
	ctx, o.Values = runValues(ctx, o.Values)
	o, err := c.withDefaults(o)
	if err != nil {
		return Record{}, err
	}
	return c.record(ctx, o, claim), nil
}

// resolve maps o.Agent (roster name, agents key, vendor:name or definition file) to its agent and applies it.
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

// SelectAndOverlay resolves ref to its agent and, for a definition, records the persona and fills unset options.
func SelectAndOverlay(ref string, agents map[string]*Schema, o *RunOptions) (name string, err error) {
	name, def, err := SelectAgent(ref, agents)
	if err != nil {
		return "", err
	}
	if def != nil {
		model := def.Model
		if model == "inherit" { // the definition has no model of its own
			model = ""
		}
		o.Persona = def
		o.Model, o.Provider, o.Effort = cmp.Or(o.Model, model), cmp.Or(o.Provider, def.Profile), cmp.Or(o.Effort, def.Effort)
	}
	return name, nil
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
	ctx, _ = runValues(ctx, p.Opts.Values)
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
	ctx, req.Values = runValues(ctx, req.Values)
	if sink == nil {
		sink = wire.Discard
	}
	req, err := checkRun(r, req)
	if err != nil {
		return wire.Result{Agent: r.Name(), Status: wire.StatusError, NotStarted: true}, retry.Fatal(r.Name(), err)
	}
	req, dropped, emulated := applyCaps(r.Caps(), req)
	ctx = WithPermissionsEnv(ctx, req.Permissions)
	res := wire.Result{Agent: r.Name(), Status: wire.StatusError, Dropped: dropped, Emulated: emulated}
	// a no-tools one-shot answers from the provider's API, so it needs no agent binary up front
	if !req.NoTools {
		if err := r.Available(); err != nil {
			res.NotStarted = true
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

// checkRun layers default permissions under the request and the outer dispatch's over it, then refuses what r can't do.
func checkRun(r Engine, req RunOptions) (RunOptions, error) {
	def, err := DefaultPermissions(r.Name())
	if err != nil {
		return req, err
	}
	req.Permissions, _ = resolvePermissions(def, req.Permissions)
	if req.Permissions, err = inheritPermissionsContext(valuesContext(context.Background(), req.Values), req.Permissions); err != nil {
		return req, err
	}
	if err = refusePersona(r.Caps(), r.Name(), req.Persona); err != nil {
		return req, err
	}
	if req.ReadOnly {
		req.Permissions = withReadOnlyRules(r.Caps(), req.Permissions)
		if r.Caps().ReadOnly == Emulated { // gated read-only: git status/diff must not rewrite .git/index; --env cannot undo it
			req.Env = append(slices.Clone(req.Env), "GIT_OPTIONAL_LOCKS=0")
		}
	}
	return req, refusePermissions(r.Caps(), r.Name(), req.Permissions)
}

type runState struct {
	res               wire.Result
	req               RunOptions
	dropped, emulated []string
	attempts          int
	failed            []*retry.Error
	started           bool // some attempt got past its guard and the engine's own pre-start checks
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
	req := s.req
	req.Values = s.req.Values.Merge(nil)
	out, err := r.Run(actx, req, cw)
	// Preserve the actual agent named by a delegating engine.
	out.Agent, out.Dropped, out.Emulated = cmp.Or(out.Agent, s.res.Agent), append(slices.Clone(s.dropped), out.Dropped...), append(slices.Clone(s.emulated), out.Emulated...)
	if _, ok := errors.AsType[notStartedError](err); ok {
		out.NotStarted = true
	}
	s.res = out
	s.started = s.started || !out.NotStarted
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
	res.NotStarted = e != nil && !s.started
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
	Values                                              proc.Values `json:"-"`
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
	OverlayUnset                                        []string    `json:"-"`                   // names an overlay file unsets; refused like OverlaySet
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
	AutoArm                                             bool               `json:"auto_arm,omitempty"` // the workspace was an auto pick: a --job child may move on a 429
	Gate                                                bool               `json:"-"`                  // claude: bypass its prompts, Permissions enforced by the injected permission-gate

	// YieldFallback lets workspace retry handle a failed dispatch before the requested chain continues.
	YieldFallback func(wire.Result, error) bool `json:"-"`
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

// packBytesNum/packBytesDen is dense code's worst case (~2.4 bytes/token): a byte budget never overruns the window.
const packBytesNum, packBytesDen = 12, 5

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

// packBudget is d's byte budget: its model window under the prompt-cap rule, then its transport cap unless a file.
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
	budget = models.PromptTokens(models.Window(model), packReserveTokens) * packBytesNum / packBytesDen
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && n < budget && !viaFile {
		budget, note = n, d.Agent.Name()+": "+s.CLI.PromptVia.String()+" prompt cap"
	}
	return budget, note
}

// packPrompt packs p.Opts.Pack for the lead, drops later dispatches it would not fit, then inlines it or stages a file.
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
