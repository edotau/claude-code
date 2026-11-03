package agent

import (
	"cmp"
	"context"

	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
	chain  Chain
	remote Engine
}

// Prepare routes the raw task before applying its rubric and claiming a dispatch record.
func (c *Client) Prepare(ctx context.Context, o RunOptions, job *Record) (*Prepared, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return nil, err
	}
	r, err := Lookup(o.Agent)
	if err != nil {
		return nil, err
	}
	p := &Prepared{}
	if o.Host != "" {
		if err := remote.ValidHost(o.Host); err != nil {
			return nil, err
		}
		r = NewRemoteEngine(o.Agent, o.Host, RemoteBin(o.RemoteBin), o.Fallback)
		p.chain, p.remote = Single(r), r
	} else if p.chain, err = c.route(ctx, r, o); err != nil {
		return nil, err
	}
	if err := RefusePersona(r.Caps(), r.Name(), o.Persona); err != nil {
		return nil, clientUsageError{err}
	}
	if o.Rubric != "" {
		if o.Prompt, err = RubricPrompt(o.Rubric, o.Prompt); err != nil {
			return nil, err
		}
	}
	p.Opts = o
	p.Record = c.record(ctx, o, job)
	return p, nil
}

// withDefaults fills the agent, fallback and launcher defaults and resolves o.Agent to its agent.
func (c *Client) withDefaults(o RunOptions) (RunOptions, error) {
	o.Agent, o.Fallback, o.Launcher = cmp.Or(o.Agent, "claude"), cmp.Or(o.Fallback, router.FallbackAll), c.Launcher
	if o.SessionID != "" {
		o.Fallback = router.FallbackNone
	}
	return o, c.resolve(&o)
}

// Record builds the dispatch record for o without routing, persona refusal or rubric (live sessions, review jobs).
func (c *Client) Record(ctx context.Context, o RunOptions, job *Record) (Record, error) {
	o, err := c.withDefaults(o)
	if err != nil {
		return Record{}, err
	}
	return c.record(ctx, o, job), nil
}

// resolve maps o.Agent — a roster name, an agents key, vendor:name or a definition file — to its agent and
// applies the definition's model, profile and effort under the caller's explicit options.
func (c *Client) resolve(o *RunOptions) error {
	if _, ok := LookupSchema(o.Agent); ok && len(o.Agents) == 0 { // a plain roster agent, not a definition selector
		return nil
	}
	agent, err := selectAndOverlay(o.Agent, o.Agents, o)
	if err != nil {
		return clientUsageError{err}
	}
	o.Agent = agent
	return nil // an explicit persona (a --retry record) survives a plain roster name
}

// selectAndOverlay resolves ref to its agent and, when it names a definition, records the persona and
// fills the unset model, profile and effort under the options already in o.
func selectAndOverlay(ref string, agents map[string]*Schema, o *RunOptions) (agent string, err error) {
	agent, def, err := SelectAgent(ref, agents)
	if err != nil {
		return "", err
	}
	if def != nil {
		o.Persona = def
		o.Model, o.Provider, o.Effort = cmp.Or(o.Model, defModel(def)), cmp.Or(o.Provider, def.Profile), cmp.Or(o.Effort, def.Effort)
	}
	return agent, nil
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

func (c *Client) route(ctx context.Context, r Engine, o RunOptions) (Chain, error) {
	if o.Provider != "" {
		if reg, err := providers.Load(); err == nil {
			if _, err := reg.Get(o.Provider); err != nil {
				return Chain{}, err
			}
		}
	}
	if ExplicitBackend(o) {
		return c.explicit(r, o)
	}
	if err := ctx.Err(); err != nil {
		return Chain{}, err
	}
	chain, _, err := RouteChainFor(ctx, o.Agent, o.Model, c.alias(o.Prompt), o.Fallback, o.Native,
		router.PickOpts{Refresh: c.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(o.WorkDir) }})
	return chain, err
}

func (c *Client) explicit(r Engine, o RunOptions) (Chain, error) {
	if o.Provider != "" && !o.Native && IsHubAgent(o.Agent) {
		reg, err := providers.Load()
		if err != nil {
			return Chain{}, err
		}
		if p, err := reg.Get(o.Provider); err != nil || p.OpencodeID != "" {
			start, err := HubStart(reg, o.Agent, o.Provider)
			if err != nil {
				return Chain{}, err
			}
			r = NewHubDispatch(o.Agent, start, reg)
		}
	}
	chain := ExplicitChain(r, o)
	if IsHubAgent(o.Agent) && o.Model == "" {
		chain = WithAlias(chain, c.alias(o.Prompt))
	}
	return chain, nil
}

func (c *Client) alias(prompt string) string {
	if c.Alias != nil {
		return c.Alias(prompt)
	}
	return ""
}

func (c *Client) record(ctx context.Context, o RunOptions, job *Record) Record {
	dir := o.WorkDir
	if o.Host != "" {
		dir = ""
	}
	rec := BuildAskRecord(ctx, o.Agent, o, job, dir, o.Background)
	rec.Request.Launcher = c.Launcher
	if job == nil {
		rec.FallbackPolicy = o.Fallback
		if o.Host != "" {
			rec.Host, rec.RemoteBin = o.Host, RemoteBin(o.RemoteBin)
		}
	}
	return rec
}

// Exec runs a prepared dispatch, leaving initial record saving and background spawning to the caller.
func (c *Client) Exec(ctx context.Context, p *Prepared, out Sink, d RunRecordedOpts) (Result, error) {
	if out == nil {
		out = Discard
	}
	if d.Notify == nil {
		d.Notify = c.Notify
	}
	ctx = withUnsetEnv(ctx, p.Opts.Unset)
	res, err := RunRecorded(ctx, &p.Record, p.chain.Want, out, d, func(ctx context.Context, sink Sink) (Result, error) {
		if p.remote != nil {
			return p.remote.Run(ctx, p.Opts, sink)
		}
		return RunWithFallback(ctx, p.chain, p.Opts, sink, c.Notify)
	})
	res.DispatchID = p.Record.ID
	return res, err
}

// Run prepares, saves and executes one dispatch; a nil output discards the caller's stream.
func (c *Client) Run(ctx context.Context, o RunOptions, out Sink) (Result, error) {
	p, err := c.Prepare(ctx, o, nil)
	if err != nil {
		return Result{Agent: cmp.Or(o.Agent, "claude"), Status: StatusError}, err
	}
	_ = p.Record.Save()
	return c.Exec(ctx, p, out, RunRecordedOpts{})
}

// SelectAndOverlay is selectAndOverlay for the batch runner.
func SelectAndOverlay(ref string, agents map[string]*Schema, o *RunOptions) (string, error) {
	return selectAndOverlay(ref, agents, o)
}
