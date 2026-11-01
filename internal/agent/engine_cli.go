package agent

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// ownProfile is the profile a run on the agent's own login bills: the registry row naming that login, else the agent.
func (s Schema) ownProfile() string {
	if reg, err := providers.Load(); err == nil {
		for _, n := range reg.Names() {
			if reg.Providers[n].Auth.Login == s.Name {
				return n
			}
		}
	}
	return s.Name
}

// target mirrors launch.Agent: the session target if it serves the agent, else its own login or a keyed one.
func (s Schema) target(req RunOptions) (providers.Target, bool, error) {
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
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		if err == nil { // an explicit login still passes the CLI's own gate
			t, note, err = s.eng().GateExplicit(req, reg, t)
		}
		return t, false, note, true, err
	}
	sel, err := providers.Select(reg, registeredSession(reg))
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
	Result Result
	Target providers.Target
	Prompt string
	Env    []string
	Argv   []string
}

// prepare resolves req's route and headless argv; the Result comes back populated even on error.
func (v cliEngine) prepare(ctx context.Context, req RunOptions) (CLIPrep, error) {
	name := v.Name()
	s := schemaFor(name)
	t, own, err := s.target(req)
	if err != nil {
		return CLIPrep{}, err
	}
	p := CLIPrep{Target: t, Result: Result{Provider: s.ownProfile(), Model: t.Model, SessionID: req.SessionID}}
	if !own {
		if t.Provider == nil { // agentTargetNote can report a non-own target with no provider resolved
			return p, &Error{Kind: KindFatal, Agent: name, Err: errors.New("agent target resolved to no provider")}
		}
		p.Result.Provider = t.Provider.Name
		p.Result.Model = v.RanModel(t)
	}
	p.Prompt, err = v.Attach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: name, Err: err}
	}
	ct := v.Before(req, t)
	if s.CLI.PromptInArgv && len(p.Prompt) > MaxPromptBytes {
		return p, &Error{Kind: KindFatal, Agent: name, Err: errPromptTooLarge(len(p.Prompt))}
	}
	h := Headless{Prompt: p.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, NoPersist: req.NoPersist, SafeMode: req.SafeMode}
	if s.CLI.DirFlag {
		h.WorkDir = req.WorkDir // cmd.Dir alone doesn't move opencode's own cwd resolution; --dir does
	}
	if own {
		p.Env, p.Argv, err = v.OwnLogin(h)
	} else {
		p.Env, p.Argv, err = s.configureHeadless(ctx, ct, h)
	}
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: name, Err: err}
	}
	v.After(&p, req)
	p.Env = append(p.Env, req.Env...) // --env goes last, after the route keys, so it wins
	return p, nil
}

func (v cliEngine) Run(ctx context.Context, req RunOptions, sink Sink) (Result, error) {
	ctx = DirectCtx(ctx, req)
	p, err := v.prepare(ctx, req)
	if err != nil {
		return p.Result, err
	}
	return v.Exec(ctx, p, req, sink)
}

// EmptyAnswer: a clean exit with no text is a failure the caller must see, not a blank success.
func EmptyAnswer(name string) error {
	return &Error{Kind: KindFatal, Agent: name,
		Err: errors.New("the CLI exited 0 with an empty answer (model returned no text)")}
}

// RunStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func RunStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, res Result, sink Sink,
	parse func(*bufio.Reader, Sink, func(), ...StreamLog) (Result, error)) (Result, error) {
	out, err := RunEventStream(ctx, name, cmd, tail, sink, parse, nil)
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

// RunEventStream runs cmd, decoding stdout via parse while streaming live; onMeaningful fires on the first model event.
func RunEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, sink Sink,
	parse func(*bufio.Reader, Sink, func(), ...StreamLog) (Result, error), onMeaningful func()) (Result, error) {
	var res Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	lg := eventLogFor(ctx)
	idle := &idleReader{r: stdout, last: time.Now().UnixNano()}
	stop := heartbeat(lg, idle, name)
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(bufio.NewReader(idle), sink, onMeaningful, lg)
	stop()
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Answer, res.Usage, res.SessionID, res.Transport, res.Structured = out.Answer, out.Usage, out.SessionID, out.Transport, out.Structured
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, name, errors.Join(werr, perr), tail)
	}
	if perr == nil && res.Answer == "" {
		return res, EmptyAnswer(name)
	}
	return res, perr
}

// heartbeatEvery is how long a child's stdout may stay quiet before a "still running" log line.
var heartbeatEvery = 30 * time.Second

// idleReader stamps the time of every read so heartbeat can tell a quiet stream from a busy one.
type idleReader struct {
	r    io.Reader
	last int64 // unix nanos of the last read, atomic
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	atomic.StoreInt64(&i.last, time.Now().UnixNano())
	return n, err
}

// heartbeat logs a "still running" line each heartbeatEvery of stdout silence until the returned stop is called.
func heartbeat(lg StreamLog, i *idleReader, name string) (stop func()) {
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

type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  struct {
			Status string `json:"status"`
		} `json:"state"`
		Tokens struct {
			Input, Output, Reasoning int
			Cache                    struct{ Read, Write int }
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// isMeaningfulOpenCodeEvent reports whether ev is real model output, not bookkeeping (step_start) or an empty part.
func isMeaningfulOpenCodeEvent(ev openCodeEvent) bool {
	switch ev.Type {
	case "text", "reasoning":
		if strings.TrimSpace(ev.Part.Text) != "" {
			return true
		}
	case "tool":
		if ev.Part.Tool != "" || ev.Part.State.Status != "" {
			return true
		}
	}
	return ev.Part.Tokens.Output > 0
}

// parseOpenCodeStream streams the root session's text parts; onMeaningful fires once on the first meaningful event.
func parseOpenCodeStream(br *bufio.Reader, sink Sink, onMeaningful func(), logs ...StreamLog) (res Result, _ error) {
	ts := TransportTracker{Start: Clock()}
	defer func() { res.Transport = ts.Result() }()
	lg := PickLog(logs)
	var texts, errs []string
	seenTools := map[string]bool{}
	fired := false
	err := DecodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev openCodeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode opencode json: %w", err)
		}
		ts.Event(Clock())
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		if !fired && onMeaningful != nil && isMeaningfulOpenCodeEvent(ev) {
			fired = true
			onMeaningful()
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text == "" {
				return false, nil
			}
			if err := sink.Emit(Event{Type: EventText, Text: ev.Part.Text, Sep: len(texts) > 0}); err != nil {
				return false, err
			}
			texts = append(texts, ev.Part.Text)
			lg.Emit("assistant", ev.Part.Text)
		case "tool", "tool_use":
			if err := emitOpenCodeTool(sink, ev, seenTools); err != nil {
				return false, err
			}
		case "step_finish":
			t := ev.Part.Tokens
			res.Usage.InputTokens += t.Input
			res.Usage.CacheReadTokens += t.Cache.Read
			res.Usage.CacheCreationTokens += t.Cache.Write
			res.Usage.OutputTokens += t.Output + t.Reasoning
			lg.Emit("result", fmt.Sprintf("step finished, tokens in=%d out=%d", t.Input, t.Output+t.Reasoning))
		case "error":
			msg := cmp.Or(ev.Error.Data.Message, ev.Error.Name, "unknown error")
			errs = append(errs, msg)
			if err := EmitError(lg, sink, msg); err != nil {
				return false, err
			}
		}
		return false, nil
	})
	if err != nil {
		return res, err
	}
	res.Answer = strings.TrimSpace(strings.Join(texts, "\n"))
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

// emitOpenCodeTool emits tool_use once per call id, then tool_result when that call completes or errors.
func emitOpenCodeTool(sink Sink, ev openCodeEvent, seen map[string]bool) error {
	p := ev.Part
	if p.Tool == "" {
		return nil
	}
	if !seen[p.CallID] {
		seen[p.CallID] = true
		if err := sink.Emit(ToolEvent(p.Tool, p.CallID)); err != nil {
			return err
		}
	}
	if p.State.Status == "completed" || p.State.Status == "error" {
		return sink.Emit(Event{Type: EventToolResult, Tool: &ToolRef{Name: p.Tool, ID: p.CallID}, Status: p.State.Status})
	}
	return nil
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

// EnsureRouter starts or finds the loopback router and returns its base URL.
func EnsureRouter(ctx context.Context) (string, error) { return ensureRouter(ctx) }

// MintSession mints a router session token for profile and pid.
func MintSession(ctx context.Context, profile string, pid int) (string, error) {
	return mintSession(ctx, profile, pid)
}

// PrepareCLI resolves req's route and headless argv for the named roster CLI.
func PrepareCLI(ctx context.Context, name string, req RunOptions) (CLIPrep, error) {
	return newCLIEngine(name).prepare(ctx, req)
}

// NewClaudeLauncher is the default Claude launcher.
func NewClaudeLauncher() Launcher { return claudeLauncher{} }
