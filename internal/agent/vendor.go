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

// ownLoginProvider is the Result.Provider for a vendor run on the credentials its own CLI stores.
const ownLoginProvider = "own-login"

// vendorTarget mirrors the shim (launch.Vendor): the session target when it can serve the vendor, else the
// vendor's own login or first keyed provider. Only --provider (or a provider:model) is the caller's provider
// choice; a slot name or model id is re-applied to the fallback provider. own is true when the vendor runs
// on its own stored login and no harness route is injected.
func vendorTarget(req Request, name string) (t providers.Target, own bool, err error) {
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		return t, false, err
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return t, false, err
	}
	ft, note, err := vendors.ResolveTarget(reg, name, sel, false)
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	own = errors.Is(err, vendors.ErrOwnLogin)
	if own {
		err = nil
	}
	if err != nil || (!own && ft.Provider == t.Provider) {
		return t, false, err // no fallback: the caller's slot or model id stands
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
	}
	return ft, own, nil
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

// vendorRunner execs a vendor CLI's headless mode wired by vendors.ConfigureHeadless.
type vendorRunner struct{ name string }

func (v vendorRunner) Name() string { return v.name }

// Caps follow each CLI's verified headless flags. Autonomous is true for all three: opencode (--auto) and
// gemini (--mode auto) map natively, and codex is already autonomous by construction with --tools on, so
// neither is a generic drop — Run prints codex's own one-line note instead.
func (v vendorRunner) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true, Autonomous: true}
}

func (v vendorRunner) Available() error {
	spec, _ := vendors.Lookup(v.name)
	_, err := paths.VendorBin(spec.Binary)
	return err
}

func (v vendorRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	t, own, err := vendorTarget(req, v.name)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: ownLoginProvider, Model: t.Model, SessionID: req.SessionID}
	if !own {
		res.Provider = t.Provider.Name
	}
	prompt, err := v.attachPrompt(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	if req.Autonomous && v.name == "codex" {
		fmt.Fprintln(os.Stderr, "ask: codex exec has no ask-step to disable; --tools already grants workspace-write")
	}
	if len(prompt) > MaxPromptBytes {
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

// attachPrompt folds --attach into the prompt for a vendor with no file-list argv: codex (prompt rides
// stdin, so the content must ride along inline) and gemini (agy has no -f flag, but resolves @path live
// only when tools are on; agy's own no-tools preamble bars it from reading @path, so inline it instead).
// opencode gets its own -f argv instead (vendors.opencodeRun), so its prompt is untouched here.
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
	return &Error{Kind: KindFatal, Agent: name, Err: errors.New("the CLI exited 0 with an empty answer (model returned no text)")}
}

// runStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func (v vendorRunner) runStream(ctx context.Context, cmd *exec.Cmd, tail *TailBuffer, res Result, stream io.Writer,
	parse func(io.Reader, io.Writer, ...eventLog) (Result, error)) (Result, error) {
	out, err := runEventStream(ctx, v.name, cmd, tail, stream, parse)
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

// runEventStream runs cmd to completion, decoding its stdout via parse while streaming text live;
// shared by vendorRunner.runStream (opencode) and RunAgy (agy's own exec, no vendors.Configure argv).
func runEventStream(ctx context.Context, name string, cmd *exec.Cmd, tail *TailBuffer, stream io.Writer,
	parse func(io.Reader, io.Writer, ...eventLog) (Result, error)) (Result, error) {
	var res Result
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, name, err, tail)
	}
	out, perr := parse(stdout, stream, eventLogFor(ctx)) // the parser writes formatted event lines to the dispatch log
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
		Text   string `json:"text"`
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

// parseOpenCodeStream streams the root session's text parts (opencode filters subagents); error events go to stdout.
// An optional eventLog gets one line per text part, step_finish usage and error.
func parseOpenCodeStream(r io.Reader, w io.Writer, logs ...eventLog) (Result, error) {
	var res Result
	lg := pickLog(logs)
	var texts, errs []string
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
	t, own, err := vendorTarget(req, "copilot")
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: ownLoginProvider, Model: models.Strip1M(t.Model)}
	if !own {
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
	if own || t.Provider.Auth.Login != "" {
		model = models.Strip1M(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
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
		if tok := vendors.CopilotLaunchToken(); tok != "" {
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
	out, err := copilotsdk.Ask(ctx, opts, copilotPrompt)
	lg := eventLogFor(ctx)
	if err != nil {
		lg.emit("error", err.Error())
		return res, err
	}
	lg.emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	if out.Model != "" {
		res.Model = out.Model
	}
	res.SessionID, res.Answer = out.SessionID, out.Text
	if _, err := io.WriteString(stream, out.Text); err != nil {
		return res, err
	}
	return res, nil
}
