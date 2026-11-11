package client

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/textutil"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
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
		if err := textutil.ValidHost(o.Host); err != nil {
			return nil, err
		}
		r = newRemoteEngine(o.Agent, o.Host, remote.Bin(o.RemoteBin), o.Fallback)
		p.remote = r
	}
	if p.Chain, err = c.route(ctx, r, o); err != nil {
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
	name, err := SelectAndOverlay(o.Agent, o.Agents, o)
	if err != nil {
		return clientUsageError{err}
	}
	o.Agent = name
	return nil // an explicit persona (a --retry record) survives a plain roster name
}

// SelectAndOverlay resolves ref to its agent and, when it names a definition, records the persona and
// fills the unset model, profile and effort under the options already in o.
func SelectAndOverlay(ref string, agents map[string]*agent.Schema, o *agent.RunOptions) (name string, err error) {
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

// route is agent.Resolve on o: o.Provider is a pinned backend, o.Lead a resolved lead, --host one remote dispatch.
func (c *Client) route(ctx context.Context, r agent.Engine, o agent.RunOptions) (agent.Chain, error) {
	chain, _, err := agent.Resolve(ctx, agent.RouteReq{Agent: o.Agent, Model: o.Model, AliasHint: c.alias(o.Prompt),
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
			rec.Host, rec.RemoteBin = o.Host, remote.Bin(o.RemoteBin)
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
func packBudget(d agent.Dispatch, req agent.RunOptions, viaFile bool) (budget int, note string) {
	s := agent.SchemaFor(d.Agent.Name())
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
	viaFile := agent.SchemaFor(lead.Agent.Name()).CLI.PackVia == agent.PackFile
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
	p.Chain = fitChain(p.Chain, len(packed.Prompt), o)
	return nil
}

// fitChain keeps the lead and every later dispatch whose budget holds n bytes; the rest become skip reasons.
func fitChain(c agent.Chain, n int, o agent.RunOptions) agent.Chain {
	out := agent.Chain{Want: c.Want, Dispatches: c.Dispatches[:1:1], Skipped: slices.Clone(c.Skipped)}
	for _, d := range c.Dispatches[1:] {
		if b, _ := packBudget(d, o, false); b < n {
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

// BuildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func BuildAskRecord(ctx context.Context, name string, req agent.RunOptions, jobRec *job.Record, workdir string, background bool) job.Record {
	if jobRec != nil {
		return *jobRec // same id: a job run continues the record it was spawned from
	}
	rec := job.ClaimRecord(name, req, time.Now())
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rec.Repo = job.RepoRoot(ctx, cwd)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec
}
