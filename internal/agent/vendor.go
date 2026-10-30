package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/vendors"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// ownProfile is the profile a run on the agent's own login bills: the registry row naming that login, else the agent.
func ownProfile(name string) string {
	if reg, err := providers.Load(); err == nil {
		for _, n := range reg.Names() {
			if reg.Providers[n].Auth.Login == name {
				return n
			}
		}
	}
	return name
}

// vendorTarget mirrors launch.Vendor: the session target if it serves the agent, else its own login or a keyed one.
func vendorTarget(req Request, name string) (providers.Target, bool, error) {
	t, own, note, err := vendorTargetNote(req, name)
	if note != "" {
		fmt.Fprintln(launch.NoticeOut, note)
	}
	return t, own, err
}

// vendorTargetNote is vendorTarget without printing: the router resolves every candidate's backend through it.
func vendorTargetNote(req Request, name string) (t providers.Target, own bool, note string, err error) {
	t, own, note, applied, err := vendorTargetBase(req, name)
	if err != nil {
		return t, own, note, err
	}
	t, modelNote := providers.ResolveAgentModel(providers.ModelAsk{Agent: name, Flag: req.Model, Target: t, Applied: applied})
	if modelNote != "" {
		note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(s string) bool { return s == "" }), "; ")
	}
	return t, own, note, nil
}

// vendorTargetBase picks the target; applied is true when t.Model already carries req.Model (no fallback respelled it).
func vendorTargetBase(req Request, name string) (t providers.Target, own bool, note string, applied bool, err error) {
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		if err == nil && name == "copilot" { // an explicit GitHub login still passes the credit gate
			from := t.Provider
			t, note, err = vendors.CopilotGate(reg, t)
			if err == nil && t.Provider != from && req.Provider != "" && req.Provider == providers.Pin(providers.PinCopilotProfile) {
				return t, false, note, true, &Error{Kind: KindQuota, Agent: "copilot", Err: errors.New(note)}
			}
			if tier := resumeSlot(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
				t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
			}
		}
		return t, false, note, true, err
	}
	sel, err := providers.Select(reg, registeredSession(reg))
	if err != nil {
		return t, false, "", true, err
	}
	ft, note, err := vendors.ResolveTarget(reg, name, sel, false)
	own = errors.Is(err, vendors.ErrOwnLogin)
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

// vendorRunner execs an agent CLI's headless mode wired by vendors.ConfigureHeadless.
type vendorRunner struct{ name string }

func (v vendorRunner) Name() string { return v.name }

// Caps follow each CLI's verified headless flags; Autonomous holds for all three (codex is autonomous by construction).
func (v vendorRunner) Caps() Caps {
	c := Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native, Tools: Native, Autonomous: Native,
		NoPersist: lv(v.name == "codex"), SafeMode: lv(v.name == "codex" || v.name == "opencode"), DebugFile: lv(v.name == "gemini"),
		Direct: Native, Env: Native, SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated}
	switch v.name { // levels verified by live probe: codex -c instructions/developer_instructions, opencode agent prompt, agy --json-schema
	case "codex":
		c.SystemPrompt, c.AppendSystemPrompt, c.OutputSchema = Native, Native, Native
	case "opencode":
		c.SystemPrompt = Native
	case "gemini":
		c.OutputSchema = Native
	}
	return c
}

func (v vendorRunner) Available() error {
	spec, _ := vendors.Lookup(v.name)
	_, err := paths.VendorBin(spec.Binary)
	return err
}

// vendorPrep is a vendor run resolved up to its child's env: the route target, the prompt and the headless argv.
type vendorPrep struct {
	res    Result
	t      providers.Target
	prompt string
	env    []string
	argv   []string
}

// prepare resolves req's route and headless argv; the Result comes back populated even on error.
func (v vendorRunner) prepare(ctx context.Context, req Request) (vendorPrep, error) {
	t, own, err := vendorTarget(req, v.name)
	if err != nil {
		return vendorPrep{}, err
	}
	p := vendorPrep{t: t, res: Result{Provider: ownProfile(v.name), Model: t.Model, SessionID: req.SessionID}}
	if !own {
		if t.Provider == nil { // vendorTargetNote can report a non-own target with no provider resolved
			return p, &Error{Kind: KindFatal, Agent: v.name, Err: errors.New("agent target resolved to no provider")}
		}
		p.res.Provider = t.Provider.Name
		if v.name == "codex" { // configureRoute swaps a non-Responses model; report the one codex runs
			p.res.Model = vendors.CodexRanModel(t)
		}
	}
	p.prompt, err = v.attachPrompt(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	if req.Autonomous && v.name == "codex" {
		fmt.Fprintln(os.Stderr, "ask: codex exec has no ask-step to disable; --tools already grants workspace-write")
	}
	// Only opencode puts the prompt in argv (vendors.opencodeRun); codex rides stdin behind `-` (codexHeadless)
	// and gemini rides agy's stdin stream-json (RunAgy/agyStdin) — neither is bound by MAX_ARG_STRLEN.
	if v.name == "opencode" && len(p.prompt) > MaxPromptBytes {
		return p, &Error{Kind: KindFatal, Agent: v.name, Err: errPromptTooLarge(len(p.prompt))}
	}
	h := vendors.Headless{Prompt: p.prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, NoPersist: req.NoPersist, SafeMode: req.SafeMode}
	if v.name == "opencode" {
		h.WorkDir = req.WorkDir // cmd.Dir alone doesn't move opencode's own cwd resolution; --dir does
	}
	if own {
		p.env, p.argv, err = vendors.OwnLoginHeadless(v.name, h)
	} else {
		ct := t
		if v.name == "gemini" { // the gateway route lists the model agy will run, not the row's slot id
			ct.Model = agyModel(req, t)
		}
		p.env, p.argv, err = vendors.ConfigureHeadless(ctx, v.name, ct, h)
	}
	if err != nil {
		return p, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	if v.name == "opencode" && req.SystemPrompt != "" {
		p.env, p.argv = opencodeSystemPrompt(p.env, p.argv, req.SystemPrompt)
	}
	p.env = append(p.env, req.Env...) // --env goes last, after the route keys, so it wins
	return p, nil
}

// agyModel is the --model agy runs for req on t; a Databricks row gets the id its gateway mode lists.
func agyModel(req Request, t providers.Target) string {
	if req.Model == "" || t.Model == "" { // the slot table's opus id isn't agy's own: GEMINI_MODEL pin, else flash-high
		t.Model = vendors.AgyDefaultModel
	}
	gt, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: "gemini", Flag: req.Model, Target: t, Applied: true})
	if providers.GeminiGatewayRow(t.Provider) {
		_, arg := vendors.AgyGatewayArg(gt.Model)
		return arg
	}
	return gt.Model
}

// agyRequest is the agy run for req: only the resolved credential env carries over, agy builds its own argv.
func (p vendorPrep) agyRequest(req Request) AgyRequest {
	return AgyRequest{
		Prompt: p.prompt, Model: agyModel(req, p.t), Effort: req.Effort, Cwd: req.WorkDir, Tools: req.Tools, Autonomous: req.Autonomous,
		DebugFile: req.DebugFile, Env: p.env, KeepEnv: req.KeepEnv, SessionID: req.SessionID, OutputSchema: req.OutputSchema,
	}
}

func (v vendorRunner) Run(ctx context.Context, req Request, sink Sink) (Result, error) {
	ctx = directCtx(ctx, req)
	p, err := v.prepare(ctx, req)
	res, prompt, env, argv := p.res, p.prompt, p.env, p.argv
	if err != nil {
		return res, err
	}
	if v.name == "gemini" {
		areq := p.agyRequest(req)
		res.Model = areq.Model
		out, err := RunAgy(ctx, areq, sink)
		res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
		if out.SessionID != "" {
			res.SessionID = out.SessionID
		}
		return res, err
	}
	spec, _ := vendors.Lookup(v.name)
	bin, err := paths.VendorBin(spec.Binary)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	var answer strings.Builder
	var banner headBuffer
	stdin := ""
	if v.name == "codex" {
		stdin = prompt // codex exec reads the prompt from stdin behind its `-` operand
	}
	codexJSON := false
	if v.name == "codex" {
		var extra []string
		if req.SystemPrompt != "" {
			extra = append(extra, "-c", "instructions="+vendors.TOMLQuote(req.SystemPrompt))
		}
		if req.AppendSystemPrompt != "" {
			extra = append(extra, "-c", "developer_instructions="+vendors.TOMLQuote(req.AppendSystemPrompt))
		}
		if req.OutputSchema != "" {
			file, err := codexSchemaFile(req.OutputSchema)
			if err != nil {
				return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
			}
			defer func() { _ = os.Remove(file) }()
			extra = append(extra, "--output-schema", file)
		}
		if codexJSON = codexHasJSON(); codexJSON {
			extra = append(extra, "--json")
		}
		argv = codexExecArgs(codexImageArgs(argv, req.Attach), extra...)
	}
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, stdin, req.KeepEnv, true)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, &banner)
	if codexJSON {
		return v.runStream(ctx, cmd, tail, res, sink, parseCodexStream)
	}
	if v.name == "opencode" {
		return v.runStream(ctx, cmd, tail, res, sink, parseOpenCodeStream)
	}
	cmd.Stdout = teeWriter(ctx, io.MultiWriter(sinkWriter(sink), &answer))
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	if err == nil && res.Answer == "" {
		return res, emptyAnswer(v.name)
	}
	return res, childErr(ctx, v.name, err, tail)
}

// attachPrompt folds --attach into the prompt for agents with no file-list argv (codex, gemini).
func (v vendorRunner) attachPrompt(prompt string, paths []string, tools bool) (string, error) {
	switch v.name {
	case "codex":
		_, rest := splitImages(paths) // images ride codex's -i flag (codexImageArgs)
		return attachInline(prompt, rest)
	case "gemini":
		if tools {
			return attachSuffix(prompt, paths), nil
		}
		return attachInline(prompt, paths)
	default:
		return prompt, nil
	}
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
func (v vendorRunner) runStream(ctx context.Context, cmd *exec.Cmd, tail *TailBuffer, res Result, sink Sink,
	parse func(io.Reader, Sink, func(), ...eventLog) (Result, error)) (Result, error) {
	out, err := runEventStream(ctx, v.name, cmd, tail, sink, parse, nil)
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

// runEventStream runs cmd, decoding stdout via parse while streaming live; onMeaningful fires on the first model event.
func runEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, sink Sink,
	parse func(io.Reader, Sink, func(), ...eventLog) (Result, error), onMeaningful func()) (Result, error) {
	var res Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(stdout, sink, onMeaningful, eventLogFor(ctx))
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
func parseOpenCodeStream(r io.Reader, sink Sink, onMeaningful func(), logs ...eventLog) (res Result, _ error) {
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.timing() }()
	lg := pickLog(logs)
	var texts, errs []string
	seenTools := map[string]bool{}
	fired := false
	dec := json.NewDecoder(r)
	for {
		var ev openCodeEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode opencode json: %w", err)
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
				continue
			}
			if err := sink.Emit(Event{Type: EventText, Text: ev.Part.Text, Sep: len(texts) > 0}); err != nil {
				return res, err
			}
			texts = append(texts, ev.Part.Text)
			lg.emit("assistant", ev.Part.Text)
		case "tool", "tool_use":
			if err := emitOpenCodeTool(sink, ev, seenTools); err != nil {
				return res, err
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
			lg.emit("error", msg)
			if err := sink.Emit(Event{Type: EventError, Text: msg}); err != nil {
				return res, err
			}
		}
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

// timing is the Transport for agents with no retry signal: Retries and RetryStatuses stay absent.
func (t *transportTracker) timing() *Transport {
	if t.events == 0 {
		return nil
	}
	return &Transport{FirstEventMs: t.first.Milliseconds(), MaxGapMs: t.gap.Milliseconds()}
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
