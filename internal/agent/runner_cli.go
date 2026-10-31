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
			t, note, err = s.eng().gateExplicit(req, reg, t)
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

// cliRunner execs an agent CLI's headless mode wired by ConfigureHeadless; the engine supplies Name, Caps, Available.
type cliRunner struct{ headlessEngine }

// newCLIRunner is the runner for a roster CLI with a headless engine: codex, gemini or opencode.
func newCLIRunner(name string) cliRunner { return cliRunner{engines[name].(headlessEngine)} }

// cliPrep is an agent run resolved up to its child's env: the route target, the prompt and the headless argv.
type cliPrep struct {
	res    Result
	t      providers.Target
	prompt string
	env    []string
	argv   []string
}

// prepare resolves req's route and headless argv; the Result comes back populated even on error.
func (v cliRunner) prepare(ctx context.Context, req RunOptions) (cliPrep, error) {
	name := v.Name()
	s := schemaFor(name)
	t, own, err := s.target(req)
	if err != nil {
		return cliPrep{}, err
	}
	p := cliPrep{t: t, res: Result{Provider: s.ownProfile(), Model: t.Model, SessionID: req.SessionID}}
	if !own {
		if t.Provider == nil { // agentTargetNote can report a non-own target with no provider resolved
			return p, &Error{Kind: KindFatal, Agent: name, Err: errors.New("agent target resolved to no provider")}
		}
		p.res.Provider = t.Provider.Name
		p.res.Model = v.ranModel(t)
	}
	p.prompt, err = v.attach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: name, Err: err}
	}
	ct := v.before(req, t)
	if s.CLI.PromptInArgv && len(p.prompt) > MaxPromptBytes {
		return p, &Error{Kind: KindFatal, Agent: name, Err: errPromptTooLarge(len(p.prompt))}
	}
	h := Headless{Prompt: p.prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, NoPersist: req.NoPersist, SafeMode: req.SafeMode}
	if s.CLI.DirFlag {
		h.WorkDir = req.WorkDir // cmd.Dir alone doesn't move opencode's own cwd resolution; --dir does
	}
	if own {
		p.env, p.argv, err = v.ownLogin(h)
	} else {
		p.env, p.argv, err = s.configureHeadless(ctx, ct, h)
	}
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: name, Err: err}
	}
	v.after(&p, req)
	p.env = append(p.env, req.Env...) // --env goes last, after the route keys, so it wins
	return p, nil
}

// agyModel is the --model agy runs for req on t; a Databricks row gets the id its gateway mode lists.
func agyModel(req RunOptions, t providers.Target) string {
	if req.Model == "" || t.Model == "" { // the slot table's opus id isn't agy's own: GEMINI_MODEL pin, else flash-high
		t.Model = AgyDefaultModel
	}
	gt, _ := ResolveAgentModel(ModelAsk{Agent: "gemini", Flag: req.Model, Target: t, Applied: true})
	if GeminiGatewayRow(t.Provider) {
		_, arg := agyGatewayArg(gt.Model)
		return arg
	}
	return gt.Model
}

// agyRequest is the agy run for req: only the resolved credential env carries over, agy builds its own argv.
func (p cliPrep) agyRequest(req RunOptions) agyRequest {
	return agyRequest{
		Prompt: p.prompt, Model: agyModel(req, p.t), Effort: req.Effort, Cwd: req.WorkDir, Tools: req.Tools, Autonomous: req.Autonomous,
		DebugFile: req.DebugFile, Env: p.env, KeepEnv: req.KeepEnv, SessionID: req.SessionID, OutputSchema: req.OutputSchema,
	}
}

func (v cliRunner) Run(ctx context.Context, req RunOptions, sink Sink) (Result, error) {
	ctx = directCtx(ctx, req)
	p, err := v.prepare(ctx, req)
	if err != nil {
		return p.res, err
	}
	return v.exec(ctx, p, req, sink)
}

// codexImageArgs adds `-i <img>` after the exec subcommand; -i is variadic, so the next token must be a flag.
func codexImageArgs(argv, attach []string) []string {
	images, _ := splitImages(attach)
	if len(images) == 0 || len(argv) == 0 || argv[0] != "exec" {
		return argv
	}
	out := []string{"exec"}
	for _, p := range images {
		out = append(out, "-i", p)
	}
	return append(out, argv[1:]...)
}

// emptyAnswer: a clean exit with no text is a failure the caller must see, not a blank success.
func emptyAnswer(name string) error {
	return &Error{Kind: KindFatal, Agent: name,
		Err: errors.New("the CLI exited 0 with an empty answer (model returned no text)")}
}

// runStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func runStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, res Result, sink Sink,
	parse func(*bufio.Reader, Sink, func(), ...eventLog) (Result, error)) (Result, error) {
	out, err := runEventStream(ctx, name, cmd, tail, sink, parse, nil)
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

// runEventStream runs cmd, decoding stdout via parse while streaming live; onMeaningful fires on the first model event.
func runEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, sink Sink,
	parse func(*bufio.Reader, Sink, func(), ...eventLog) (Result, error), onMeaningful func()) (Result, error) {
	var res Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(bufio.NewReader(stdout), sink, onMeaningful, eventLogFor(ctx))
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Answer, res.Usage, res.SessionID, res.Transport, res.Structured = out.Answer, out.Usage, out.SessionID, out.Transport, out.Structured
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, name, errors.Join(werr, perr), tail)
	}
	if perr == nil && res.Answer == "" {
		return res, emptyAnswer(name)
	}
	return res, perr
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
func parseOpenCodeStream(br *bufio.Reader, sink Sink, onMeaningful func(), logs ...eventLog) (res Result, _ error) {
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.result() }()
	lg := pickLog(logs)
	var texts, errs []string
	seenTools := map[string]bool{}
	fired := false
	err := decodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev openCodeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode opencode json: %w", err)
		}
		ts.event(clock())
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
			lg.emit("assistant", ev.Part.Text)
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
			lg.emit("result", fmt.Sprintf("step finished, tokens in=%d out=%d", t.Input, t.Output+t.Reasoning))
		case "error":
			msg := cmp.Or(ev.Error.Data.Message, ev.Error.Name, "unknown error")
			errs = append(errs, msg)
			if err := emitError(lg, sink, msg); err != nil {
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
		if err := sink.Emit(toolEvent(p.Tool, p.CallID)); err != nil {
			return err
		}
	}
	if p.State.Status == "completed" || p.State.Status == "error" {
		return sink.Emit(Event{Type: EventToolResult, Tool: &ToolRef{Name: p.Tool, ID: p.CallID}, Status: p.State.Status})
	}
	return nil
}

// headBuffer keeps the first 4 KiB of a stream: codex prints the session id in its stderr banner.
type headBuffer struct{ buf []byte }

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := 4<<10 - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (h *headBuffer) String() string { return string(h.buf) }

// sessionFromBanner reads codex exec's `session id: <id>` line, so a new run's envelope can be resumed.
func sessionFromBanner(banner string) string {
	for line := range strings.SplitSeq(banner, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "session id:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// directCtx carries req.Direct to Configure (skip the router).
func directCtx(ctx context.Context, req RunOptions) context.Context {
	if req.Direct {
		return WithDirect(ctx)
	}
	return ctx
}
