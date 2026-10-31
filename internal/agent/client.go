package agent

import (
	"cmp"
	"context"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Client assembles and executes recorded agent dispatches.
type Client struct {
	Launcher Launcher
	Tier     func(task string) string
	Refresh  func(ctx context.Context, account string) bool
	Notify   func(string)
	Agents   map[string]*Schema
}

// NewClient accepts launch, classification and refresh seams without depending on their CLI implementations.
func NewClient(l Launcher, tier func(string) string, refresh func(context.Context, string) bool, notify func(string)) *Client {
	return &Client{Launcher: l, Tier: tier, Refresh: refresh, Notify: notify}
}

// Prepared is a routed, recorded dispatch that has not started.
type Prepared struct {
	Opts   RunOptions
	Record Record
	chain  Chain
	remote Runner
}

// Prepare routes the raw task before applying its rubric and claiming a dispatch record.
func (c *Client) Prepare(ctx context.Context, o RunOptions, job *Record) (*Prepared, error) {
	o.Agent, o.Fallback, o.Launcher = cmp.Or(o.Agent, "claude"), cmp.Or(o.Fallback, FallbackAll), c.Launcher
	if o.SessionID != "" {
		o.Fallback = FallbackNone
	}
	r, err := Lookup(o.Agent)
	if err != nil {
		return nil, err
	}
	p := &Prepared{}
	if o.Host != "" {
		if err := tailscale.ValidHost(o.Host); err != nil {
			return nil, err
		}
		r = NewRemoteRunner(o.Agent, o.Host, RemoteBin(o.RemoteBin), o.Fallback)
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

type clientUsageError struct{ error }

func (clientUsageError) ExitCode() int { return 2 }

func (c *Client) route(ctx context.Context, r Runner, o RunOptions) (Chain, error) {
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
	chain, _, err := RouteChainFor(ctx, o.Agent, o.Model, c.tier(o.Prompt), o.Fallback, o.Native,
		router.Opts{Refresh: c.Refresh, WorkRepo: func() bool { return router.IsWorkRepo(o.WorkDir) }})
	return chain, err
}

func (c *Client) explicit(r Runner, o RunOptions) (Chain, error) {
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
			r = NewHubLeg(o.Agent, start, reg)
		}
	}
	chain := ExplicitChain(r, o)
	if IsHubAgent(o.Agent) && o.Model == "" {
		chain = WithTier(chain, c.tier(o.Prompt))
	}
	return chain, nil
}

func (c *Client) tier(prompt string) string {
	if c.Tier != nil {
		return c.Tier(prompt)
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
func (c *Client) Exec(ctx context.Context, p *Prepared, out Sink, d DispatchOpts) (Result, error) {
	if out == nil {
		out = Discard
	}
	if d.Notify == nil {
		d.Notify = c.Notify
	}
	res, err := Dispatch(ctx, &p.Record, p.chain.Want, out, d, func(ctx context.Context, sink Sink) (Result, error) {
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
	return c.Exec(ctx, p, out, DispatchOpts{})
}
