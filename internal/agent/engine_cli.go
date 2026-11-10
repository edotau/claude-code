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

// Target mirrors launch.Agent: the session target if it serves the agent, else its own login or a keyed one.
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
	t, modelNote := ResolveAgentModel(ModelAsk{Agent: s.Name, Flag: req.Model, Target: t, Applied: applied})
	if modelNote != "" {
		note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(n string) bool { return n == "" }), "; ")
	}
	return t, own, note, nil
}

// targetBase picks the target; applied is true when t.Model already carries req.Model (no fallback respelled it).
func (s Schema) targetBase(req RunOptions) (t providers.Target, own bool, note string, applied bool, err error) {
	if reg, _ := providers.Load(); req.Provider == "" && !namesProvider(reg, req.Model) { // the vault pin outranks the session
		if req.Provider, err = PinnedProfile(s.Name); err != nil {
			return t, false, "", true, err
		}
	}
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		if err == nil { // an explicit login still passes the CLI's own gate
			t, note, err = s.Eng().GateExplicit(req, reg, t)
		}
		return t, false, note, true, err
	}
	sel, err := providers.Select(reg, providers.SessionLead(reg, false))
	if err != nil {
		return t, false, "", true, err
	}
	ft, note, err := s.ResolveTarget(reg, sel, false)
	own = errors.Is(err, ErrOwnLogin)
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
		if t.Provider == nil { // agentTargetNote can report a non-own target with no provider resolved
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
	if n := cmp.Or(s.CLI.PromptMaxBytes, s.CLI.PromptVia.MaxBytes()); n > 0 && len(p.Prompt) > n {
		return p, retry.Fatal(name, PromptTooLarge(len(p.Prompt), n))
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
	return CheckRouteNames(p.Env, req.OverlaySet)
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
	out, err := RunEventStream(ctx, name, cmd, tail, sink, parse, nil)
	return withParsed(res, out), err
}

// withParsed copies the parser's answer, usage, transport and structured output into res; an empty session id keeps res's.
func withParsed(res, out wire.Result) wire.Result {
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	res.SessionID = cmp.Or(out.SessionID, res.SessionID)
	return res
}

// RunEventStream runs cmd, decoding stdout via parse while streaming live; onMeaningful fires on the first model event.
func RunEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *retry.TailBuffer, sink wire.Sink,
	parse func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error), onMeaningful func()) (wire.Result, error) {
	var res wire.Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	agentconfig.RecordOverlayDebug(name, cmd.Env)
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	lg := wire.EventLogFor(ctx)
	idle := &idleReader{src: EOFReader{R: stdout}, last: time.Now().UnixNano()}
	stop := heartbeat(lg, idle, name)
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(bufio.NewReader(idle), sink, onMeaningful, lg)
	stop()
	werr := ReapChild(cmd, &idle.src, perr)
	res = withParsed(res, out)
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, name, errors.Join(werr, perr), tail)
	}
	if perr == nil && res.Answer == "" && len(res.Structured) == 0 { // a structured-only answer is an answer
		return res, EmptyAnswer(name)
	}
	return res, perr
}

// heartbeatEvery is how long a child's stdout may stay quiet before a "still running" log line.
var heartbeatEvery = 30 * time.Second

// EOFReader wraps a child's stdout and records that it reached EOF, the cue for ReapChild.
type EOFReader struct {
	R   io.Reader
	eof atomic.Bool
}

func (e *EOFReader) Read(p []byte) (int, error) {
	n, err := e.R.Read(p)
	if err == io.EOF {
		e.eof.Store(true)
	}
	return n, err
}

// ReapChild waits for cmd once its stdout was parsed; a parse error before EOF kills it, else it drains and waits.
func ReapChild(cmd *exec.Cmd, out *EOFReader, perr error) error {
	if perr != nil && !out.eof.Load() && cmd.Cancel != nil { // after EOF the child is exiting: killing would mask its status
		_ = cmd.Cancel() // a dead consumer or bad stream must not leave the child running to --timeout
	} else {
		_, _ = io.Copy(io.Discard, out) // no drain after a kill: a grandchild holding stdout would block it
	}
	return cmd.Wait()
}

// idleReader stamps the time of every read so heartbeat can tell a quiet stream from a busy one.
type idleReader struct {
	src  EOFReader
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
