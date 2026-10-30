package agent

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// copilotRunner drives the copilot CLI through the SDK with the BYOK block; tools are denied unless asked.
type copilotRunner struct{}

func (copilotRunner) Name() string { return "copilot" }
func (copilotRunner) Caps() Caps {
	return Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, Tools: Native, SessionID: Native, Autonomous: Native,
		Memory: Native, Direct: Native, Env: Native, SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated}
}
func (copilotRunner) Available() error { _, err := paths.VendorBin("copilot"); return err }

// applyCopilotToken puts the own-login launch token, then the harness account's env token, on the SDK's token field.
func applyCopilotToken(opts *copilotsdk.Options, own bool, env []string) error {
	if own {
		tok, err := CopilotLaunchToken()
		if err != nil {
			return err
		}
		if tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	for _, kv := range env { // the harness's chosen account rides the SDK's own token field, not the stored login
		if tok, ok := strings.CutPrefix(kv, CopilotTokenEnv+"="); ok && tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	return nil
}

func (copilotRunner) Run(ctx context.Context, req Request, sink Sink) (Result, error) {
	ctx = directCtx(ctx, req)
	t, own, err := vendorTargetCtx(ctx, req, "copilot")
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
	var route *copilotRegistryRoute
	switch {
	case own:
	case CopilotRegistryEligible(req.Direct, t):
		if route, err = prepareCopilotRegistry(ctx, t); err != nil {
			return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
		}
		defer route.close()
	default:
		env, _, err = Configure(ctx, "copilot", t, "")
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
	if route != nil {
		route.apply(&opts)
	}
	// the SDK merges os.Environ itself, so blank other profiles' credentials the launch does not set itself
	opts.Env = append(blankForeignCredentials(ctx, opts.Env, req.KeepEnv), req.Env...) // --env goes last so it wins
	if err := applyCopilotToken(&opts, own, env); err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	// the SDK never resolves @<path>: images ride SDK file attachments, text files the inline block
	images, rest := splitImages(req.Attach)
	if images, err = absPaths(images); err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	copilotPrompt, err := attachInline(req.Prompt, rest)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	out, err := copilotAsk(ctx, opts, copilotPrompt, images)
	lg := eventLogFor(ctx)
	if err != nil {
		lg.emit("error", err.Error())
		if (own || t.Provider.Auth.Login == "copilot") && Classify("copilot", err, false).Kind == KindQuota {
			creds := credentialValues(env, opts.GitHubToken)
			return copilotQuotaHandoff(req, res, out, err, creds)
		}
		res.SessionID, res.Answer = cmp.Or(out.SessionID, res.SessionID), out.Text // partial text + session survive a failure
		if strings.TrimSpace(out.Text) != "" {
			// streamed bytes mark progress, so Run does not retry over a partial
			if serr := sink.Emit(Event{Type: EventText, Text: out.Text}); serr != nil {
				err = errors.Join(err, serr)
			}
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
	if err := sink.Emit(Event{Type: EventText, Text: out.Text}); err != nil {
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

// directCtx carries req.Direct to Configure (skip the router).
func directCtx(ctx context.Context, req Request) context.Context {
	if req.Direct {
		return WithDirect(ctx)
	}
	return ctx
}

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = copilotsdk.Ask

// absPaths makes each path absolute (the SDK sends them to a runtime with its own working directory).
func absPaths(paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out[i] = abs
	}
	return out, nil
}
