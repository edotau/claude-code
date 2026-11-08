package client

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Client assembles and executes recorded agent dispatches.
type Client struct {
	Launcher agent.Launcher
	Alias    func(task string) string
	Refresh  func(ctx context.Context, account string) bool
	Notify   func(string)
}

// NewClient accepts classification and refresh seams without depending on their CLI implementations.
func NewClient(alias func(string) string, refresh func(context.Context, string) bool, notify func(string)) *Client {
	return &Client{Launcher: agent.DefaultLauncher, Alias: alias, Refresh: refresh, Notify: notify}
}

// Prepared is a routed, recorded dispatch that has not started.
type Prepared struct {
	Opts   agent.RunOptions
	Record job.Record
	Chain  agent.Chain
	remote agent.Engine
	pack   *packFile // the pack an agent reads from the workdir; written by Exec, removed after the run
}

// Prepare routes the raw task before applying its rubric and claiming a dispatch record.
func (c *Client) Prepare(ctx context.Context, o agent.RunOptions, claim *job.Record) (*Prepared, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return nil, err
	}
	r, err := agent.Lookup(o.Agent)
	if err != nil {
		return nil, err
	}
	p := &Prepared{}
	if o.Host != "" {
		if err := remote.ValidHost(o.Host); err != nil {
			return nil, err
		}
		r = NewRemoteEngine(o.Agent, o.Host, RemoteBin(o.RemoteBin), o.Fallback)
		p.Chain, p.remote = agent.Single(r), r
	} else if p.Chain, err = c.route(ctx, r, o); err != nil {
		return nil, err
	}
	if err := agent.RefusePersona(r.Caps(), r.Name(), o.Persona); err != nil {
		return nil, clientUsageError{err}
	}
	if o.Rubric != "" {
		if o.Prompt, err = agent.RubricPrompt(o.Rubric, o.Prompt); err != nil {
			return nil, err
		}
	}
	p.Opts = o
	p.Record = c.record(ctx, o, claim)
	if o.Pack != nil {
		if p.remote != nil {
			return nil, clientUsageError{errors.New("--dirs/--files/--diff: not with --host")}
		}
		if err := c.packPrompt(ctx, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// withDefaults fills the agent, fallback and launcher defaults and resolves o.Agent to its agent.
func (c *Client) withDefaults(o agent.RunOptions) (agent.RunOptions, error) {
	o.Agent, o.Fallback, o.Launcher = cmp.Or(o.Agent, "claude"), cmp.Or(o.Fallback, router.FallbackAll), c.Launcher
	if o.SessionID != "" {
		o.Fallback = router.FallbackNone
	}
	return o, c.resolve(&o)
}

// Record builds the dispatch record for o without routing, persona refusal or rubric (live sessions, review jobs).
func (c *Client) Record(ctx context.Context, o agent.RunOptions, claim *job.Record) (job.Record, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return job.Record{}, err
	}
	return c.record(ctx, o, claim), nil
}

// resolve maps o.Agent — a roster name, an agents key, vendor:name or a definition file — to its agent and
// applies the definition's model, profile and effort under the caller's explicit options.
func (c *Client) resolve(o *agent.RunOptions) error {
	if _, ok := agent.LookupSchema(o.Agent); ok && len(o.Agents) == 0 { // a plain roster agent, not a definition selector
		return nil
	}
	name, err := selectAndOverlay(o.Agent, o.Agents, o)
	if err != nil {
		return clientUsageError{err}
	}
	o.Agent = name
	return nil // an explicit persona (a --retry record) survives a plain roster name
}

// selectAndOverlay resolves ref to its agent and, when it names a definition, records the persona and
// fills the unset model, profile and effort under the options already in o.
func selectAndOverlay(ref string, agents map[string]*agent.Schema, o *agent.RunOptions) (name string, err error) {
	name, def, err := agent.SelectAgent(ref, agents)
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
func defModel(def *agent.Schema) string {
	if def.Model == "inherit" {
		return ""
	}
	return def.Model
}

type clientUsageError struct{ error }

func (clientUsageError) ExitCode() int { return 2 }

func (c *Client) route(ctx context.Context, r agent.Engine, o agent.RunOptions) (agent.Chain, error) {
	var reg *providers.Registry // loaded once when a provider is named; explicit reuses it
	if o.Provider != "" {
		var err error
		if reg, err = providers.Load(); err != nil {
			return agent.Chain{}, err
		}
		if _, err := reg.Get(o.Provider); err != nil {
			return agent.Chain{}, err
		}
	}
	if agent.ExplicitBackend(o) && o.Lead == "" {
		return c.explicit(reg, r, o)
	}
	if err := ctx.Err(); err != nil {
		return agent.Chain{}, err
	}
	chain, _, err := agent.RouteChainFor(ctx, o.Agent, o.Model, c.alias(o.Prompt), o.Fallback, o.Native,
		router.PickOpts{Lead: o.Lead, Refresh: c.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(o.WorkDir) }})
	return chain, err
}

// explicit builds the chain for a named backend; reg is route's registry, non-nil whenever o.Provider is set.
func (c *Client) explicit(reg *providers.Registry, r agent.Engine, o agent.RunOptions) (agent.Chain, error) {
	if o.Provider != "" && !o.Native && agent.IsHubAgent(o.Agent) {
		if p, err := reg.Get(o.Provider); err != nil || p.OpencodeID != "" {
			start, err := agent.HubStart(reg, o.Agent, o.Provider)
			if err != nil {
				return agent.Chain{}, err
			}
			r = agent.NewHubDispatch(o.Agent, start, reg)
		}
	}
	chain := agent.ExplicitChain(r, o)
	if agent.IsHubAgent(o.Agent) && o.Model == "" {
		chain = agent.WithAlias(chain, c.alias(o.Prompt))
	}
	return chain, nil
}

func (c *Client) alias(prompt string) string {
	if c.Alias != nil {
		return c.Alias(prompt)
	}
	return ""
}

func (c *Client) record(ctx context.Context, o agent.RunOptions, claim *job.Record) job.Record {
	dir := o.WorkDir
	if o.Host != "" {
		dir = ""
	}
	rec := BuildAskRecord(ctx, o.Agent, o, claim, dir, o.Background)
	rec.Request.Launcher = c.Launcher
	if claim == nil {
		rec.FallbackPolicy = o.Fallback
		if o.Host != "" {
			rec.Host, rec.RemoteBin = o.Host, RemoteBin(o.RemoteBin)
		}
	}
	return rec
}

// Exec runs a prepared dispatch, leaving initial record saving and background spawning to the caller.
func (c *Client) Exec(ctx context.Context, p *Prepared, out wire.Sink, d job.RunRecordedOpts) (wire.Result, error) {
	if out == nil {
		out = wire.Discard
	}
	if d.Notify == nil {
		d.Notify = c.Notify
	}
	ctx = agent.WithUnsetEnv(ctx, p.Opts.Unset)
	if p.pack != nil {
		if err := p.pack.write(); err != nil {
			res := wire.Result{Agent: p.Chain.Want, Status: wire.StatusError, DispatchID: p.Record.ID}
			p.Record.Finish(res, err, time.Since(p.Record.Time))
			if serr := p.Record.Save(); serr != nil && d.Notify != nil {
				d.Notify(fmt.Sprintf("dispatch %s record not saved, it stays at running: %v", p.Record.ID, serr))
			}
			return res, err
		}
		defer p.pack.remove()
	}
	res, err := job.RunRecorded(ctx, &p.Record, p.Chain.Want, out, d, func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		if p.remote != nil {
			return p.remote.Run(ctx, p.Opts, sink)
		}
		return agent.RunWithFallback(ctx, p.Chain, p.Opts, sink, d.Notify)
	})
	res.DispatchID = p.Record.ID
	return res, err
}

// Run prepares, saves and executes one dispatch; a nil output discards the caller's stream.
func (c *Client) Run(ctx context.Context, o agent.RunOptions, out wire.Sink) (wire.Result, error) {
	p, err := c.Prepare(ctx, o, nil)
	if err != nil {
		return wire.Result{Agent: cmp.Or(o.Agent, "claude"), Status: wire.StatusError}, err
	}
	if err := p.Record.Save(); err != nil && c.Notify != nil { // fail-open: the log never gates the run
		c.Notify(fmt.Sprintf("dispatch %s record not saved before the run: %v", p.Record.ID, err))
	}
	return c.Exec(ctx, p, out, job.RunRecordedOpts{})
}

// SelectAndOverlay is selectAndOverlay for the batch runner.
func SelectAndOverlay(ref string, agents map[string]*agent.Schema, o *agent.RunOptions) (string, error) {
	return selectAndOverlay(ref, agents, o)
}

const streamBuffer = 1024

var streamStall = 2 * time.Minute

// ErrStreamAbandoned is the cancellation cause when a stream stays full beyond streamStall.
var ErrStreamAbandoned = errors.New("agent: stream consumer abandoned")

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
		s.cancel(ErrStreamAbandoned)
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

// Stream runs a dispatch and returns its buffered event stream.
func (c *Client) Stream(ctx context.Context, o agent.RunOptions) <-chan wire.Event {
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
