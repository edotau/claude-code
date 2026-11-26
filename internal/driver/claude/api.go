// The one-shot HTTP calls of claude, routed through the shared stdlib streaming client in internal/agent.
package claude

import (
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// claudeOneShot is claude's tool-less one-shot via Anthropic or OpenAI HTTP; ok is false for cli auth.
func claudeOneShot(ctx context.Context, req agent.RunOptions, sink wire.Sink) (res wire.Result, ok bool, err error) {
	_, t, err := agent.ResolveRequestTarget(req)
	if err != nil {
		return wire.Result{}, true, err
	}
	p := t.Provider
	res = wire.Result{Provider: p.Name, Model: t.Model}
	if p.Auth.Method() == providers.AuthCLI {
		return res, false, nil
	}
	effort, err := oneShotEffort(req, t)
	if err != nil {
		return res, true, retry.Fatal("claude", err)
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, false)
	if err != nil {
		return wire.Result{}, true, err
	}
	if _, isAnth := providers.AnthropicRoute(p, t.Model); !isAnth {
		if err := providers.ClaudeCodeError(p, t.Model); err != nil {
			return res, true, retry.Fatal("claude", err)
		}
		if _, servable, why := providers.WireFor(p, t.Model); !servable {
			return res, true, retry.Fatal("claude", errors.New(why))
		}
	}
	httpReq := req
	httpReq.Prompt, httpReq.Effort = prompt, effort
	res, err = agent.OneShotHTTP(ctx, t, httpReq, sink)
	res.Dropped = oneShotDropped(req)
	logAPIRun(ctx, res, err)
	return res, true, err
}

// oneShotDropped names the flags that need a process: the HTTP one-shot has none to carry --env or write a debug file.
func oneShotDropped(req agent.RunOptions) []string {
	var out []string
	if len(req.Env) > 0 {
		out = append(out, "--env")
	}
	if req.DebugFile != "" {
		out = append(out, "--debug-file="+req.DebugFile)
	}
	return out
}

// oneShotEffort is the flag's effort, else the stored record's, spelled as the CLI path spells it.
func oneShotEffort(req agent.RunOptions, t providers.Target) (string, error) {
	st, err := agent.ResolveSettings(agentid.Claude, req, t, true)
	if err != nil {
		return "", err
	}
	return claudeEffort(st.Effort)
}

// logAPIRun writes the one-shot's answer, usage or error to the dispatch log (the stream has no event boundaries).
func logAPIRun(ctx context.Context, res wire.Result, err error) {
	lg := wire.EventLogFor(ctx)
	if res.Answer != "" {
		lg.Emit("assistant", res.Answer)
	}
	if err != nil {
		lg.Emit("error", err.Error())
		return
	}
	lg.Emit("result", fmt.Sprintf("tokens in=%d out=%d", res.Usage.InputTokens, res.Usage.OutputTokens))
}
