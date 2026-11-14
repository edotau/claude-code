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
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/models"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
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
	t, own, note, err := s.targetNote(req)
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	return t, own, err
}

// targetNote is target without printing: the router resolves every candidate's backend through it.
func (s Schema) targetNote(req RunOptions) (t providers.Target, own bool, note string, err error) {
	t, own, note, applied, err := s.targetBase(req)
	if err != nil {
		return t, own, note, err
	}
	t = servedTarget(s.Name, t)
	t, modelNote := ResolveAgentModel(ModelAsk{Agent: s.Name, Flag: req.Model, Target: t, Applied: applied})
	if modelNote != "" {
		note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(n string) bool { return n == "" }), "; ")
	}
	return t, own, note, nil
}

// targetBase picks the target; applied is true when t.Model already carries req.Model (no fallback respelled it).
func (s Schema) targetBase(req RunOptions) (t providers.Target, own bool, note string, applied bool, err error) {
	reg, loadErr := providers.Load()
	if req.Provider == "" && !namesProvider(reg, req.Model) { // the vault pin outranks the session
		if req.Provider, err = PinnedProfile(s.Name); err != nil {
			return t, false, "", true, err
		}
	}
	if loadErr != nil {
		return t, false, "", true, loadErr
	}
	reg, t, err = resolveIn(reg, req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		if err == nil { // an explicit login still passes the CLI's own gate
			t, note, err = s.Eng().GateExplicit(req, reg, t)
		}
		return t, false, note, true, err
	}
	return s.sessionTarget(reg, t)
}

// sessionTarget is targetBase with no provider asked: the session lead's target when it can serve s, else t.
func (s Schema) sessionTarget(reg *providers.Registry, t providers.Target) (providers.Target, bool, string, bool, error) {
	sel, err := providers.Select(reg, providers.SessionLead(reg, false))
	if err != nil {
		return t, false, "", true, err
	}
	ft, note, err := s.ResolveTarget(reg, sel, false)
	own := errors.Is(err, ErrOwnLogin)
	if own {
		err = nil
	}
	if err != nil || (!own && ft.Provider == t.Provider) {
		return t, false, note, true, err // no fallback: the caller's slot or model id stands
	}
	return ft, own, note, false, nil
}

// namesProvider is true for the provider:model form, where the prefix is a registered provider.
func namesProvider(reg *providers.Registry, spec string) bool {
	if reg == nil {
		return false
	}
	name, _, ok := strings.Cut(spec, ":")
	_, found := reg.Providers[name]
	return ok && found
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

// checkPromptCap refuses a prompt over the agent's transport cap.
func (s Schema) checkPromptCap(prompt string) error {
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && len(prompt) > n {
		return PromptTooLarge(len(prompt), n)
	}
	return nil
}

// prepare resolves req's route and headless argv; the Result comes back populated even on error.
func (v cliEngine) prepare(ctx context.Context, req RunOptions) (CLIPrep, error) {
	name := v.Name()
	s := SchemaFor(name)
	t, own, err := s.Target(req)
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
		p.Env, p.Argv, err = v.OwnLogin(h)
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

// renderSettings applies the agent's settings to a configured p, then refuses a route name an overlay sets.
func (v cliEngine) renderSettings(ctx context.Context, p *CLIPrep, req RunOptions, t providers.Target) (err error) {
	st, err := ResolveSettings(v.Name(), req, t, true) // headless: Launch.Args stay off; a bad record fails the run
	if err != nil {
		return err
	}
	if p.Env, p.Argv, p.Emulated, p.Cleanup, err = v.RenderSettings(ctx, st, p.Env, p.Argv); err != nil {
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
	out, err := RunEventStream(ctx, name, cmd, tail, sink, parse, StreamOpts{})
	return withParsed(res, out), err
}

// withParsed copies the parser's answer, usage, transport, structured output and model into res; empty ids keep res's.
func withParsed(res, out wire.Result) wire.Result {
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	res.SessionID = cmp.Or(out.SessionID, res.SessionID)
	res.Model = cmp.Or(out.Model, res.Model)
	return res
}

// StreamOpts are RunEventStream's options: Idle kills a silent child; OnMeaningful fires on the first model event.
type StreamOpts struct {
	Idle         *IdleTimeout
	OnMeaningful func()
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
	stop := heartbeat(lg, idle, name)
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(bufio.NewReader(idle), sink, opts.meaningful(), lg)
	stop()
	opts.Idle.Stop()
	werr := reapChild(cmd, &idle.src, perr)
	res = withParsed(res, out)
	return streamResult(ctx, name, res, perr, werr, opts.Idle, tail)
}

// streamResult picks RunEventStream's outcome: an idle kill, a delivered result, a child failure, an empty answer.
func streamResult(ctx context.Context, name string, res wire.Result, perr, werr error, idle *IdleTimeout,
	tail *retry.TailBuffer) (wire.Result, error) {
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
func heartbeat(lg wire.StreamLog, i *idleReader, name string) (stop func()) {
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
					lg.Emit("heartbeat", fmt.Sprintf("%s still running, no output for %s", name, quiet))
				}
			}
		}
	}()
	return func() { close(done) }
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
