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

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
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
		fmt.Fprintln(os.Stderr, note)
	}
	return t, own, err
}

// vendorTargetNote is vendorTarget without printing: the router resolves every candidate's backend through it.
func vendorTargetNote(req Request, name string) (t providers.Target, own bool, note string, err error) {
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		if err == nil && name == "copilot" { // an explicit GitHub login still passes the credit gate
			from := t.Provider
			t, note, err = vendors.CopilotGate(reg, t)
			if tier := resumeSlot(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
				t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
			}
		}
		return t, false, note, err
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return t, false, "", err
	}
	ft, note, err := vendors.ResolveTarget(reg, name, sel, false)
	own = errors.Is(err, vendors.ErrOwnLogin)
	if own {
		err = nil
	}
	if err != nil || (!own && ft.Provider == t.Provider) {
		return t, false, note, err // no fallback: the caller's slot or model id stands
	}
	switch {
	case req.Model == "":
	case slices.Contains(models.Slots, req.Model):
		if ft.Provider != nil && ft.Provider.Models[req.Model] != "" {
			m := ft.Provider.Models[req.Model]
			ft.Model = m
		}
	default:
		ft.Model = req.Model
		if ft.Provider != nil { // a typed id is respelled the way the fallback provider serves it, never copied
			ft.Model = ft.Provider.WireModel(models.Strip1M(req.Model))
		}
	}
	return ft, own, note, nil
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
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true, Autonomous: true}
}

func (v vendorRunner) Available() error {
	spec, _ := vendors.Lookup(v.name)
	_, err := paths.VendorBin(spec.Binary)
	return err
}

func (v vendorRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	ctx = directCtx(ctx, req)
	t, own, err := vendorTarget(req, v.name)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: ownProfile(v.name), Model: t.Model, SessionID: req.SessionID}
	if !own {
		if t.Provider == nil { // vendorTargetNote can report a non-own target with no provider resolved
			return res, &Error{Kind: KindFatal, Agent: v.name, Err: errors.New("agent target resolved to no provider")}
		}
		res.Provider = t.Provider.Name
	}
	prompt, err := v.attachPrompt(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	if req.Autonomous && v.name == "codex" {
		fmt.Fprintln(os.Stderr, "ask: codex exec has no ask-step to disable; --tools already grants workspace-write")
	}
	// Only opencode puts the prompt in argv (vendors.opencodeRun); codex rides stdin behind `-` (codexHeadless)
	// and gemini rides agy's stdin stream-json (RunAgy/agyStdin) — neither is bound by MAX_ARG_STRLEN.
	if v.name == "opencode" && len(prompt) > MaxPromptBytes {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: errPromptTooLarge(len(prompt))}
	}
	h := vendors.Headless{Prompt: prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach}
	if v.name == "opencode" {
		h.WorkDir = req.WorkDir // cmd.Dir alone doesn't move opencode's own cwd resolution; --dir does
	}
	var env, argv []string
	if own {
		env, argv, err = vendors.OwnLoginHeadless(v.name, h)
	} else {
		env, argv, err = vendors.ConfigureHeadless(ctx, v.name, t, h)
	}
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	if v.name == "gemini" {
		// agy builds its own argv (stream-json over stdin); only the resolved credential env carries over.
		model := t.Model
		if req.Model == "" || model == "" { // the slot table's opus id isn't agy's own: GEMINI_MODEL pin, else flash-high
			model = cmp.Or(providers.Pin(providers.PinGeminiModel), AgyDefaultModel)
		}
		res.Model = model
		out, err := RunAgy(ctx, AgyRequest{
			Prompt: prompt, Model: model, Effort: req.Effort, Cwd: req.WorkDir, Tools: req.Tools, Autonomous: req.Autonomous,
			Env: env, KeepEnv: req.KeepEnv, SessionID: req.SessionID,
		}, stream)
		res.Answer, res.Usage = out.Answer, out.Usage
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
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, stdin, req.KeepEnv, true)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, &banner)
	if v.name == "opencode" {
		return v.runStream(ctx, cmd, tail, res, stream, parseOpenCodeStream)
	}
	cmd.Stdout = teeWriter(ctx, io.MultiWriter(stream, &answer))
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
		return attachInline(prompt, paths)
	case "gemini":
		if tools {
			return attachSuffix(prompt, paths), nil
		}
		return attachInline(prompt, paths)
	default:
		return prompt, nil
	}
}

// emptyAnswer: a clean exit with no text is a failure the caller must see, not a blank success.
func emptyAnswer(name string) error {
	return &Error{Kind: KindFatal, Agent: name,
		Err: errors.New("the CLI exited 0 with an empty answer (model returned no text)")}
}

// runStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func (v vendorRunner) runStream(ctx context.Context, cmd *exec.Cmd, tail *TailBuffer, res Result, stream io.Writer,
	parse func(io.Reader, io.Writer, func(), ...eventLog) (Result, error)) (Result, error) {
	out, err := runEventStream(ctx, v.name, cmd, tail, stream, parse, nil)
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

// runEventStream runs cmd, decoding stdout via parse while streaming live; onMeaningful fires on the first model event.
func runEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, stream io.Writer,
	parse func(io.Reader, io.Writer, func(), ...eventLog) (Result, error), onMeaningful func()) (Result, error) {
	var res Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	// the parser writes formatted event lines to the dispatch log and fires onMeaningful on its first real event.
	out, perr := parse(stdout, stream, onMeaningful, eventLogFor(ctx))
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Answer, res.Usage, res.SessionID = out.Answer, out.Usage, out.SessionID
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
		Text  string `json:"text"`
		Tool  string `json:"tool"`
		State struct {
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
func parseOpenCodeStream(r io.Reader, w io.Writer, onMeaningful func(), logs ...eventLog) (Result, error) {
	var res Result
	lg := pickLog(logs)
	var texts, errs []string
	fired := false
	dec := json.NewDecoder(r)
	for {
		var ev openCodeEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode opencode json: %w", err)
		}
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
			if len(texts) > 0 {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return res, err
				}
			}
			if _, err := io.WriteString(w, ev.Part.Text); err != nil {
				return res, err
			}
			texts = append(texts, ev.Part.Text)
			lg.emit("assistant", ev.Part.Text)
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
		}
	}
	res.Answer = strings.TrimSpace(strings.Join(texts, "\n"))
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
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

// copilotRunner drives the copilot CLI through the SDK with the BYOK block; tools are denied unless asked.
type copilotRunner struct{}

func (copilotRunner) Name() string { return "copilot" }
func (copilotRunner) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, Tools: true, SessionID: true,
		Autonomous: true, Memory: true}
}
func (copilotRunner) Available() error { _, err := paths.VendorBin("copilot"); return err }

func (copilotRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	ctx = directCtx(ctx, req)
	t, own, err := vendorTarget(req, "copilot")
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: ownProfile("copilot"), Model: models.Strip1M(t.Model)}
	if !own {
		if t.Provider == nil {
			return res, &Error{Kind: KindFatal, Agent: "copilot", Err: errors.New("agent target resolved to no provider")}
		}
		res.Provider = t.Provider.Name
	}
	var env []string
	if !own {
		env, _, err = vendors.Configure(ctx, "copilot", t, "")
		if err != nil {
			return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
		}
	}
	bin, err := paths.VendorBin("copilot")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	var model string
	if own || (t.Provider != nil && t.Provider.Auth.Login != "") {
		model = copilotModelID(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	// An explicit --effort overrides the pin; there is no per-call --context, so the pin alone decides it.
	effort := cmp.Or(req.Effort, providers.Pin(providers.PinCopilotEffort))
	opts := copilotsdk.Options{
		CLIPath: bin, Env: env, KeepEnv: req.KeepEnv, Model: model, ReasoningEffort: effort,
		Context: providers.Pin(providers.PinCopilotContext), WorkingDirectory: req.WorkDir,
		AllowAllTools: req.Tools, UseLoggedInUser: true, Autonomous: req.Autonomous, EnableMemory: req.EnableMemory,
		SessionID: req.SessionID,
	}
	if own {
		tok, err := vendors.CopilotLaunchToken()
		if err != nil {
			return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
		}
		if tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	for _, kv := range env { // the harness's chosen account rides the SDK's own token field, not the stored login
		if tok, ok := strings.CutPrefix(kv, vendors.CopilotTokenEnv+"="); ok && tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	// @<path> works for the copilot CLI directly, but not through the SDK's own prompt parsing (live-confirmed:
	// the SDK never resolved the reference, unlike the CLI's `-p` form) — fall back to the inline block.
	copilotPrompt, err := attachInline(req.Prompt, req.Attach)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	out, err := copilotAsk(ctx, opts, copilotPrompt)
	lg := eventLogFor(ctx)
	if err != nil {
		lg.emit("error", err.Error())
		if (own || t.Provider.Auth.Login == "copilot") && Classify("copilot", err, false).Kind == KindQuota {
			creds := credentialValues(env, opts.GitHubToken)
			return copilotQuotaHandoff(ctx, req, res, out, err, creds, copilotRunner{}.Run, stream)
		}
		return res, err
	}
	lg.emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	if out.Model != "" {
		res.Model = out.Model
	}
	res.SessionID, res.Answer = out.SessionID, out.Text
	if strings.TrimSpace(out.Text) == "" {
		return res, emptyAnswer("copilot")
	}
	if _, err := io.WriteString(stream, out.Text); err != nil {
		return res, err
	}
	return res, nil
}

// copilotModelID is id as Copilot's catalog spells it (dotted versions); [1m] is dropped, an unknown id is kept.
func copilotModelID(id string) string {
	id = models.Strip1M(id)
	if canonical, _, ok := models.Resolve(id); ok {
		return models.Wire("dotted", canonical)
	}
	return id
}

// directCtx carries req.Direct to vendors.Configure (skip the router).
func directCtx(ctx context.Context, req Request) context.Context {
	if req.Direct {
		return vendors.WithDirect(ctx)
	}
	return ctx
}

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = copilotsdk.Ask
