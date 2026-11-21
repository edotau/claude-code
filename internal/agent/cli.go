package agent

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
)

// OwnProfile is the profile a run on the agent's own login bills: the registry row naming that login, else the agent.
func (s Schema) OwnProfile() string {
	if reg, err := providers.Load(); err == nil {
		for _, n := range reg.Names() {
			if reg.Providers[n].Auth.Login == s.Name {
				return n
			}
		}
	}
	return s.Name
}

// Target picks the session target if it serves the agent, else its own login or a keyed one.
func (s Schema) Target(req RunOptions) (providers.Target, bool, error) {
	return s.TargetCtx(context.Background(), req)
}

// targetNote is target without printing: the router resolves every candidate's backend through it.
func (s Schema) targetNote(req RunOptions) (t providers.Target, own bool, note string, err error) {
	return s.targetNoteIn(context.Background(), req)
}

// targetNoteIn is targetNote under ctx, so the ask's re-pick exclusions reach the pick.
func (s Schema) targetNoteIn(ctx context.Context, req RunOptions) (t providers.Target, own bool, note string, err error) {
	return s.targetNoteCtx(valuesContext(ctx, req.Values), req)
}

// targetPeek is targetNote for a caller that only reads the target: an auto rotation pick is peeked, not stamped.
func (s Schema) targetPeek(req RunOptions) (t providers.Target, own bool, note string, err error) {
	return s.targetNoteCtx(PeekContext(valuesContext(context.Background(), req.Values)), req)
}

func (s Schema) targetNoteCtx(ctx context.Context, req RunOptions) (t providers.Target, own bool, note string, err error) {
	req.Account = cmp.Or(req.Account, overlayAccount(ctx))
	t, own, note, applied, err := s.targetBase(req)
	if err != nil {
		return t, own, note, err
	}
	t = servedTarget(s.Name, t)
	t, modelNote, err := s.routedModel(ctx, req, t, applied)
	if err != nil {
		return t, own, note, err
	}
	return t, own, joinNotes(note, modelNote), nil
}

// overlayAccount is the overlay's account key, read once at the ask entry; "" when none or unreadable.
func overlayAccount(ctx context.Context) string {
	ov, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil {
		return ""
	}
	return cmp.Or(ov.Account, ov.Profile)
}

// routedModel is t with the model ResolveRun spells for t's row: a typed model, else auto over the pin, else the pin.
func (s Schema) routedModel(ctx context.Context, req RunOptions, t providers.Target, applied bool) (providers.Target, string, error) {
	t, done := typedModelTarget(req, t, applied)
	if done {
		return t, "", nil
	}
	reg, err := providers.Load()
	if err != nil {
		return t, "", err
	}
	q := s.routedQuery(req, t)
	r, err := ResolveRun(reg, QueryFor(ctx, q), time.Now())
	switch {
	case errors.Is(err, ErrPoolExhausted):
		return t, "", err // a pinned auto with every pool model held stops like a typed one
	case errors.Is(err, providers.ErrModelSpec):
		return t, "", modelUsage(q.Model, err) // an old <profile>:<model> pin is refused, never swapped for the row's (D6)
	case errors.Is(err, ErrModelRoute) && q.Model == "":
		return t, "", nil // a record model the row cannot serve keeps the row's own tier model
	case errors.Is(err, ErrModelRoute):
		return t, "", modelUsage(q.Model, err)
	case err != nil:
		return t, "", err
	}
	t.Model, t.OneM = r.Model, r.OneM
	return t, r.Note, nil
}

// typedModelTarget is t carrying a typed model as is; done is true when nothing is left to resolve.
func typedModelTarget(req RunOptions, t providers.Target, applied bool) (_ providers.Target, done bool) {
	auto := IsAuto(req.Model)
	if t.Provider != nil && (!applied || req.Model == "" || req.CellModel || auto) {
		return t, false
	}
	if t.Provider == nil && req.Model != "" && !auto && !slices.Contains(models.AliasNames, req.Model) {
		t.Model = providers.ConcreteModel(req.Agent, nil, req.Model)
	}
	return t, true
}

// routedQuery is req's model query on t's row.
func (s Schema) routedQuery(req RunOptions, t providers.Target) ModelQuery {
	q := ModelQuery{Agent: s.Name, Model: req.Model, Profile: t.Provider.Name, Task: req.Prompt, Tools: req.Tools}
	if req.CellModel {
		q.Model = "" // a bare ladder cell is the classifier's alias, not a typed model
	}
	if req.AutoModel && q.Model == "" {
		q.Model = AutoModel
	}
	return q
}

// leadProfile is an untyped request's lead: the overlay account's row, else the pin when no session row serves (D3).
func (s Schema) leadProfile(ctx context.Context, reg *providers.Registry, account, model string) (string, error) {
	if reg != nil && account != "" {
		if p := accountRow(reg, account); p != nil {
			return p.Name, nil
		}
	}
	if reg != nil {
		q := QueryFor(ctx, ModelQuery{Agent: s.Name, Model: model})
		if p, _, err := geminiModelRow(reg, q); err != nil {
			return "", err
		} else if p != nil {
			return p.Name, nil
		}
		if p := ownPinRow(reg, q); p != nil {
			return p.Name, nil
		}
		if p := sessionRow(reg, q); p != nil {
			if ok, _ := rowServes(s.Name, p); ok {
				return "", nil
			}
		}
	}
	return PinnedProfile(ctx, s.Name)
}

// targetBase picks the target; applied is true when t.Model already carries req.Model (no fallback respelled it).
func (s Schema) targetBase(req RunOptions) (t providers.Target, own bool, note string, applied bool, err error) {
	ctx := valuesContext(context.Background(), req.Values)
	reg, loadErr := providers.Load()
	if req.Provider == "" && req.pinnedRow(reg) == nil {
		if req.Provider, err = s.leadProfile(ctx, reg, req.Account, req.Model); err != nil {
			return t, false, "", true, err
		}
	}
	if loadErr != nil {
		return t, false, "", true, loadErr
	}
	reg, t, err = resolveIn(reg, req)
	if err != nil || req.Provider != "" || req.pinnedRow(reg) != nil {
		if err == nil { // an explicit login still passes the CLI's own gate
			t, note, err = s.Eng().GateExplicit(ctx, req, reg, t)
		}
		return t, false, note, true, err
	}
	return s.sessionTargetContext(ctx, req, reg, t)
}

func (s Schema) sessionTargetContext(ctx context.Context, req RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, bool, string, bool, error) {
	sel, err := providers.Select(reg, providers.SessionLeadContext(ctx, reg, false))
	if err != nil {
		return t, false, "", true, err
	}
	ft, note, err := s.ResolveTarget(ctx, req, reg, sel, false)
	own := errors.Is(err, ErrOwnLogin)
	if own {
		err = nil
	}
	if err != nil || (!own && ft.Provider == t.Provider) {
		return t, false, note, true, err // no fallback: the caller's slot or model id stands
	}
	return ft, own, note, false, nil
}

// pinnedRow is the registry row a router-pinned cell names, nil when the request carries none.
func (o RunOptions) pinnedRow(reg *providers.Registry) *providers.Provider {
	return reg.Row(o.CellRoute.Profile)
}

// cliEngine execs an agent CLI's headless mode wired by ConfigureHeadless; the engine supplies Name, Caps, Available.
type cliEngine struct{ HeadlessDriver }

// newCLIEngine is the agent for a roster CLI with a headless engine: codex, gemini or opencode.
func newCLIEngine(name string) cliEngine { return cliEngine{mustDriver(name).(HeadlessDriver)} }

// CLIPrep is an agent run resolved up to its child's env: the route target, the prompt and the headless argv.
type CLIPrep struct {
	Result wire.Result
	Target providers.Target
	Prompt string
	Env    []string
	Argv   []string
	// Cleanup removes RenderSettings' per-run files; Emulated names the settings a shim honoured.
	Cleanup  func()
	Emulated []string
}

// PrepareLaunch resolves headless settings and renders into p; SDK adapters pass nil and use the settings directly.
func PrepareLaunch(ctx context.Context, d Driver, req RunOptions, t providers.Target, p *CLIPrep) (Settings, error) {
	st, err := ResolveSettings(d.Name(), req, t, true)
	if err == nil && p != nil {
		p.Env, p.Argv, p.Emulated, p.Cleanup, err = d.RenderSettings(ctx, st, p.Env, p.Argv)
	}
	return st, err
}

// checkPromptCap refuses a prompt over the agent's transport cap.
func (s Schema) checkPromptCap(prompt string) error {
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && len(prompt) > n {
		return PromptTooLarge(len(prompt), n)
	}
	return nil
}

// prepare resolves req's route and headless argv; the Result comes back populated even on error.
func (v cliEngine) prepare(ctx context.Context, req RunOptions) (CLIPrep, error) {
	ctx, req.Values = RunValues(ctx, req.Values)
	name := v.Name()
	s := SchemaFor(name)
	t, own, err := s.TargetCtx(ctx, req)
	if err != nil {
		return CLIPrep{}, err
	}
	p := CLIPrep{Target: t, Result: wire.Result{Provider: s.OwnProfile(), Model: t.Model, SessionID: req.SessionID}}
	if !own {
		if t.Provider == nil { // targetNote can report a non-own target with no provider resolved
			return p, retry.Fatal(name, errors.New("agent target resolved to no provider"))
		}
		p.Result.Provider = t.Provider.Name
		p.Result.Model = v.RanModel(t)
	}
	p.Prompt, err = v.Attach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return p, retry.Fatal(name, err)
	}
	ct := v.Before(req, t)
	if err := s.checkPromptCap(p.Prompt); err != nil {
		return p, retry.Fatal(name, err)
	}
	h := cliHeadless(s, p.Prompt, req)
	if own {
		p.Env, p.Argv, err = v.OwnLogin(ctx, h)
	} else {
		p.Env, p.Argv, err = s.ConfigureHeadless(ctx, ct, h)
	}
	if err != nil {
		return p, retry.Fatal(name, err)
	}
	v.After(&p, req)
	if err := v.renderSettings(ctx, &p, req, ct); err != nil {
		return p, retry.Fatal(name, err)
	}
	p.Env = append(p.Env, req.Env...) // --env goes last, after the route keys, so it wins
	return p, nil
}

func (v cliEngine) renderSettings(ctx context.Context, p *CLIPrep, req RunOptions, t providers.Target) error {
	if _, err := PrepareLaunch(ctx, v, req, t, p); err != nil {
		return err
	}
	return CheckRouteNames(p.Env, req.OverlaySet, req.OverlayUnset)
}

func cliHeadless(s Schema, prompt string, req RunOptions) Headless {
	h := Headless{Prompt: prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, NoPersist: req.NoPersist, SafeMode: req.SafeMode}
	if s.CLI.DirFlag {
		h.WorkDir = req.WorkDir // cmd.Dir alone doesn't move opencode's own cwd resolution; --dir does.
	}
	return h
}

func (v cliEngine) Run(ctx context.Context, req RunOptions, sink wire.Sink) (wire.Result, error) {
	ctx = DirectCtx(ctx, req)
	p, err := v.prepare(ctx, req)
	if p.Cleanup != nil {
		defer p.Cleanup()
	}
	if err != nil {
		p.Result.NotStarted = true // prepare fails before any child process starts
		return p.Result, err
	}
	return v.Exec(ctx, p, req, sink)
}

// EmptyAnswer: a clean exit with no text is a failure the caller must see, not a blank success.
func EmptyAnswer(name string) error {
	return retry.Fatal(name, errors.New("the CLI exited 0 with an empty answer (model returned no text)"))
}

// RunStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func RunStream(ctx context.Context, name string, cmd *exec.Cmd, tail *retry.TailBuffer, res wire.Result, sink wire.Sink,
	parse func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error)) (wire.Result, error) {
	return RunStreamWith(ctx, name, cmd, tail, res, sink, parse, StreamOpts{})
}

// RunStreamWith is RunStream with stream options, such as an idle watch.
func RunStreamWith(ctx context.Context, name string, cmd *exec.Cmd, tail *retry.TailBuffer, res wire.Result, sink wire.Sink,
	parse func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error), opts StreamOpts) (wire.Result, error) {
	out, err := RunEventStream(ctx, name, cmd, tail, sink, parse, opts)
	return withParsed(res, out), err
}

// withParsed copies the parser's answer, usage, transport, structured output and model into res; empty ids keep res's.
func withParsed(res, out wire.Result) wire.Result {
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	res.SessionID = cmp.Or(out.SessionID, res.SessionID)
	res.Model = cmp.Or(out.Model, res.Model)
	res.Reported = cmp.Or(strings.TrimSpace(out.Model), strings.TrimSpace(out.Reported)) // never res.Model: launch default
	return res
}

// StreamOpts are RunEventStream's options: Idle kills a silent child; OnMeaningful fires on the first model event.
type StreamOpts struct {
	Idle         *IdleTimeout
	OnMeaningful func()
	Cause        func() error // a failure the child's stderr reported; it beats the idle and exit errors
}

// meaningful is the callback the parser fires on its first model event: OnMeaningful, then it marks Idle as spoken.
func (o StreamOpts) meaningful() func() {
	if o.Idle == nil {
		return o.OnMeaningful
	}
	return func() {
		if o.OnMeaningful != nil {
			o.OnMeaningful()
		}
		o.Idle.Spoke()
	}
}

func (o StreamOpts) cause() error {
	if o.Cause == nil {
		return nil
	}
	return o.Cause()
}

// RunEventStream runs cmd, decoding stdout via parse while streaming live; a tripped Idle beats the exit error.
func RunEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *retry.TailBuffer, sink wire.Sink,
	parse func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error), opts StreamOpts) (wire.Result, error) {
	var res wire.Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	agentconfig.RecordOverlayDebug(name, cmd.Env)
	if err := cmd.Start(); err != nil {
		return res, ChildErr(ctx, name, err, tail)
	}
	opts.Idle.bind(cmd.Cancel) // a silent child must not hold the run to --timeout
	lg := wire.EventLogFor(ctx)
	idle := &idleReader{src: eofReader{R: stdout}, last: time.Now().UnixNano()}
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	stop := heartbeat(lg, idle, name, id)
	defer stop() // a panicking parser must not leave the ticker goroutine running
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(bufio.NewReader(idle), sink, opts.meaningful(), lg)
	stop()
	opts.Idle.Stop()
	werr := reapChild(cmd, &idle.src, perr)
	res = withParsed(res, out)
	return streamResult(ctx, name, res, perr, werr, opts, tail)
}

// streamResult picks RunEventStream's outcome: an idle kill, a delivered result, a child failure, an empty answer.
func streamResult(ctx context.Context, name string, res wire.Result, perr, werr error, opts StreamOpts,
	tail *retry.TailBuffer) (wire.Result, error) {
	idle := opts.Idle
	if cerr := opts.cause(); cerr != nil {
		return res, cerr
	}
	if serr := idle.Err(); serr != nil {
		return res, serr
	}
	if perr == nil && idle.HasDelivered() { // the result arrived whole: the kill of a lingering child is no failure
		return res, nil
	}
	if werr != nil || ctx.Err() != nil {
		return res, ChildErr(ctx, name, errors.Join(werr, perr), tail)
	}
	if perr == nil && res.Answer == "" && len(res.Structured) == 0 { // a structured-only answer is an answer
		return res, EmptyAnswer(name)
	}
	return res, perr
}

// heartbeatEvery is how long a child's stdout may stay quiet before a "still running" log line.
var heartbeatEvery = 30 * time.Second

// eofReader wraps a child's stdout and records that it reached EOF, the cue for reapChild.
type eofReader struct {
	R   io.Reader
	eof atomic.Bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.R.Read(p)
	if err == io.EOF {
		e.eof.Store(true)
	}
	return n, err
}

// reapChild waits for cmd once its stdout was parsed; a parse error before EOF kills it, else it drains and waits.
func reapChild(cmd *exec.Cmd, out *eofReader, perr error) error {
	if perr != nil && !out.eof.Load() && cmd.Cancel != nil { // after EOF the child is exiting: killing would mask its status
		_ = cmd.Cancel() // a dead consumer or bad stream must not leave the child running to --timeout
	} else {
		_, _ = io.Copy(io.Discard, out) // no drain after a kill: a grandchild holding stdout would block it
	}
	return cmd.Wait()
}

// idleReader stamps the time of every read so heartbeat can tell a quiet stream from a busy one.
type idleReader struct {
	src  eofReader
	last int64 // unix nanos of the last read, atomic
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.src.Read(p)
	atomic.StoreInt64(&i.last, time.Now().UnixNano())
	return n, err
}

// heartbeat logs a "still running" line each heartbeatEvery of stdout silence until the returned stop is called.
func heartbeat(lg wire.StreamLog, i *idleReader, name, id string) (stop func()) {
	if lg == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				quiet := time.Since(time.Unix(0, atomic.LoadInt64(&i.last))).Round(time.Second)
				if quiet >= heartbeatEvery {
					lg.Emit("heartbeat", heartbeatText(name, id, quiet))
				}
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

// heartbeatText is the progress line of dispatch id, else the plain still-running text.
func heartbeatText(name, id string, quiet time.Duration) string {
	if rec, err := LoadRecord(id); err == nil && rec.Status == "running" {
		return ProgressOf(rec, time.Now()).Line(progressWidth)
	}
	return fmt.Sprintf("%s still running, no output for %s", name, quiet)
}

// HeadBuffer keeps the first 4 KiB of a stream: codex prints the session id in its stderr banner.
type HeadBuffer struct{ buf []byte }

func (h *HeadBuffer) Write(p []byte) (int, error) {
	if room := 4<<10 - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (h *HeadBuffer) String() string { return string(h.buf) }

// DirectCtx carries req.Direct to Configure (skip the router).
func DirectCtx(ctx context.Context, req RunOptions) context.Context {
	ctx = valuesContext(ctx, req.Values)
	if req.Direct {
		return WithDirect(ctx)
	}
	return ctx
}

// PrepareCLI resolves req's route and headless argv for the named roster CLI.
func PrepareCLI(ctx context.Context, name string, req RunOptions) (CLIPrep, error) {
	return newCLIEngine(name).prepare(ctx, req)
}

// ResolveRequestTarget applies --provider then --model to the registry; an aliased Claude id takes the session's pin.
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
	sel, err := providers.Select(reg, requestLead(reg, req))
	if sel.Provider == nil || (err != nil && req.Model == "") {
		return nil, providers.Target{}, err
	}
	if req.Model == "" {
		return reg, sel.Aliases[models.Opus], nil
	}
	if p := req.pinnedRow(reg); p != nil { // a dispatch cell names its own model on its row
		return reg, servedTarget(cmp.Or(req.Agent, "claude"), providers.PlainTarget(p, strings.TrimSpace(req.Model))), nil
	}
	if t, ok, err := agyOwnTarget(req, sel.Provider); ok || err != nil {
		return reg, t, err
	}
	if t, ok := aliasedTarget(sel, req.Model); ok {
		return reg, t, nil
	}
	if t, ok := familyTarget(req, sel.Provider); ok {
		return reg, t, nil
	}
	t := providers.PlainTarget(sel.Provider, strings.TrimSpace(req.Model))
	return reg, servedTarget(cmp.Or(req.Agent, "claude"), t), nil
}

// requestLead is the row a request selects: a folded auto cell's own row, else --provider, else the session's.
func requestLead(reg *providers.Registry, req RunOptions) string {
	if p := req.pinnedRow(reg); p != nil && req.Model == "" {
		return p.Name
	}
	return cmp.Or(req.Provider, providers.SessionLeadContext(valuesContext(context.Background(), req.Values), reg, false))
}

// familyTarget is a typed GPT family name (sol) as the concrete id its row resolves to; else ok is false.
func familyTarget(req RunOptions, row *providers.Provider) (providers.Target, bool) {
	if !providers.IsFamily(req.Model) {
		return providers.Target{}, false
	}
	return providers.Target{Provider: row, Model: providers.ConcreteModel(req.Agent, row, req.Model)}, true
}

// aliasedTarget is the alias row or canonical-id target for model; else ok is false.
func aliasedTarget(sel providers.Selection, model string) (providers.Target, bool) {
	if t, ok := sel.Aliases[model]; ok {
		return t, true
	}
	if t, ok := aliasTarget(sel, model); ok {
		return t, true
	}
	return providers.Target{}, false
}

// agyOwnTarget is agy's own model typed on its Google row, spelled as agy takes it; an effort it lacks errors.
func agyOwnTarget(req RunOptions, p *providers.Provider) (t providers.Target, ok bool, err error) {
	if !agentid.Routing(req.Agent).EffortSuffix || p.Auth.Login != agentid.Gemini {
		return t, false, nil
	}
	model := strings.ToLower(req.Model)
	base, effort, cut := agentid.SplitNativeEffort(model)
	if !cut {
		base, effort = model, ""
	}
	// AcceptedEfforts rejects an empty base just as it rejects the unsplit "-high" spelling.
	accepted := models.AcceptedEfforts(agentid.Gemini, base)
	if len(accepted) == 0 || effort == "" && models.FamilyKind(base) == models.FamilyGemini {
		return t, false, nil
	}
	if effort != "" && !slices.Contains(accepted, effort) {
		return t, false, &UsageError{Msg: fmt.Sprintf("--model %s: %s takes effort %s on agy, not %q", req.Model, base, strings.Join(accepted, ", "), effort)}
	}
	t = providers.Target{Provider: p, Model: p.WireModel(base)}
	if effort != "" {
		t.Model += "-" + effort
	}
	return t, true, nil
}

// servedTarget is t with its model in preflight's spelling: a bare id the workspace serves runs as databricks-<id>.
func servedTarget(agentName string, t providers.Target) providers.Target {
	if t.Provider != nil && t.Provider.Preset == providers.PresetDatabricks {
		t.Model = launchWire(agentName, t.Provider, t.Model)
	}
	return t
}

// profileAliasRequest resolves an alias cell pinned to a row like --provider <row> --model <alias>.
func profileAliasRequest(reg *providers.Registry, req RunOptions) RunOptions {
	if p := req.pinnedRow(reg); p != nil && slices.Contains(models.AliasNames, req.Model) {
		req.Provider, req.CellRoute = p.Name, route.Route{}
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
	if ok && t.Model == AutoModel { // a router alias names no model: the typed id stays exact
		return providers.Target{}, false
	}
	if ok && oneM {
		t.OneM = true
	}
	return t, ok
}
