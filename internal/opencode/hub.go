package opencode

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeLogins is a seam over OpencodeLogins, so tests never shell out to opencode auth.
var opencodeLogins = OpencodeLogins

// HubStart resolves the provider a hub agent's first dispatch targets: explicitProvider when the caller set
// one (forced, regardless of login or quota), else the first opencodeLadder entry that has a login,
// falling back to the ladder's first entry at all so the page stays editable with no login anywhere.
// The ladder orders opencode's own paid backends only — the free Zen catalog (opus excluded, alias- and
// work-repo-gated) is the per-alias sub-pick's concern (route.Table.Pick), not this cross-alias provider page.
func HubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	if explicitProvider != "" {
		p, err := reg.Get(explicitProvider)
		if err != nil {
			return nil, err
		}
		if p.OpencodeID == "" {
			return nil, fmt.Errorf("%s: provider %s has no opencodeID, so the hub cannot route to it", agentName, p.Name)
		}
		return p, nil
	}
	var first *providers.Provider
	var logins []string // probed once, on the first ladder entry that resolves
	probed := false
	for _, id := range reg.Routing.Opencode.LadderOrder() {
		p, err := opencodeLadderProvider(reg, id)
		if err != nil {
			continue
		}
		if first == nil {
			first = p
		}
		if !probed {
			logins, probed = opencodeLogins(), true
		}
		if slices.Contains(logins, OpencodeCredID(p.OpencodeID)) {
			return p, nil
		}
	}
	if first != nil {
		return first, nil
	}
	return nil, fmt.Errorf("%s: no opencode ladder configured (set opencodeLadder with `claude-code vault edit`)", agentName)
}

// opencodeLadderProvider resolves one opencodeLadder id to its registry provider: a provider whose
// opencodeID matches directly (github-copilot, openai, nvidia, ...), else a synthetic provider for
// opencode's own plan (opencode-go), which carries no registry entry.
func opencodeLadderProvider(reg *providers.Registry, id string) (*providers.Provider, error) {
	for _, name := range reg.Names() {
		if p := reg.Providers[name]; p.OpencodeID == id {
			return p, nil
		}
	}
	if OpencodeCredID(id) == "opencode" {
		return &providers.Provider{Name: id, OpencodeID: id}, nil
	}
	return nil, fmt.Errorf("opencode ladder id %q: no provider has opencodeID %q", id, id)
}

// hubDispatch is one opencode-routed ladder step: `opencode run -m <opencodeID>/<model>` on provider, under
// the exposed agent name — so Result.Agent reads the same across every dispatch of the chain, and only
// Result.Provider shows which account actually answered.
type hubDispatch struct {
	agent    string
	provider *providers.Provider
	reg      *providers.Registry
	// firstOutput, when non-zero, fails the dispatch if nothing streams within it — the free Zen dispatch's stall guard.
	firstOutput time.Duration
}

// NewHubDispatch builds a hub dispatch Agent for agentName on provider; reg resolves a provider:model override.
func NewHubDispatch(agentName string, provider *providers.Provider, reg *providers.Registry) agent.HubEngine {
	return hubDispatch{agent: agentName, provider: provider, reg: reg}
}

func (l hubDispatch) Name() string { return l.agent }

// HubProvider is the account this step dispatches on.
func (l hubDispatch) HubProvider() *providers.Provider { return l.provider }

// WithFirstOutput sets the free dispatch's stall watchdog.
func (l hubDispatch) WithFirstOutput(d time.Duration) agent.Engine {
	l.firstOutput = d
	return l
}

// Caps mirrors cliEngine's: every RunOptions field the opencode CLI can carry.
func (l hubDispatch) Caps() agent.Caps {
	return agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native, Tools: agent.Native, Autonomous: agent.Native,
		SafeMode: agent.Native, Permissions: agent.Native, Direct: agent.Native, Env: agent.Native, SystemPrompt: agent.Native, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated}
}

// Available requires the opencode binary and an opencode login for this dispatch's provider; a login-less
// ladder step reports why here instead of failing mid-exec.
func (l hubDispatch) Available() error {
	if _, err := paths.AgentBin("opencode"); err != nil {
		return err
	}
	_, err := opencodeCredential(l.provider.OpencodeID)
	return err
}

func (l hubDispatch) Run(ctx context.Context, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	model := l.resolveModel(req.Model)
	res := agent.Result{Provider: l.provider.Name, Model: model, SessionID: req.SessionID}
	if model == "" {
		return res, &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: fmt.Errorf("%s: no model resolved", l.provider.Name)}
	}
	if len(req.Prompt) > agent.MaxPromptBytes {
		return res, &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: agent.PromptTooLarge(len(req.Prompt))}
	}
	bin, env, argv, err := l.prepareLaunch(req, model)
	if err != nil {
		return res, &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: err}
	}
	env, plugged, err := opencodeApplyPlugin(env, req)
	if err != nil {
		return res, &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: err}
	}
	cctx := ctx
	var onFirstByte, onStep func()
	if l.firstOutput > 0 {
		var stop func()
		cctx, onFirstByte, onStep, stop = armFirstOutput(ctx, l.firstOutput)
		defer stop()
	}
	host := newHookHost(req)
	env = withHostedMarker(env, host)
	if plugged {
		host.dropPlugin() // the plugin blocks for real; the host's detect-only copies would double-run them
	}
	argv, err = host.before(argv, req.Prompt)
	if err != nil {
		return res, &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: err}
	}
	cmd, tail := agent.ChildCmd(cctx, bin, argv, env, req.WorkDir, "", req.KeepEnv, true)
	out, runErr := agent.RunEventStream(cctx, l.agent, cmd, tail, sink, host.parser(onStep), onFirstByte)
	defer func() { host.after(res) }()
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, l.runError(cctx, req, res.Answer, runErr)
}

// runError is the hub run's outcome: a stall after first output, then a tools-off answer that is only tool calls.
func (l hubDispatch) runError(cctx context.Context, req agent.RunOptions, answer string, runErr error) error {
	if l.firstOutput > 0 && errors.Is(context.Cause(cctx), agent.ErrStalled) {
		return &agent.Error{Kind: agent.KindFatal, Agent: l.agent, Err: agent.ErrStalled}
	}
	if runErr != nil {
		return runErr
	}
	return refuseToolCallText(l.agent, req, answer)
}

func (l hubDispatch) prepareLaunch(req agent.RunOptions, model string) (string, []string, []string, error) {
	bin, err := paths.AgentBin("opencode")
	if err != nil {
		return "", nil, nil, err
	}
	h := agent.Headless{Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, WorkDir: req.WorkDir, SafeMode: req.SafeMode}
	cred, err := opencodeCredential(l.provider.OpencodeID)
	if err != nil {
		return "", nil, nil, err
	}
	env, argv := opencodeRunArgv(l.provider.OpencodeID+"/"+model, h)
	env = append(env, cred...)
	if req.SystemPrompt != "" {
		env, argv = opencodeSystemPrompt(env, argv, req.SystemPrompt)
	}
	if err := agent.CheckRouteNames(env, req.OverlaySet); err != nil {
		return "", nil, nil, err
	}
	env, err = opencodeApplyPermissions(env, req)
	if err != nil {
		return "", nil, nil, err
	}
	argv = opencodePolicyArgv(argv, req)
	env = append(env, req.Env...) // --env goes last so it wins
	return bin, env, argv, nil
}

// armFirstOutput is the stall watchdog: cctx is cancelled with agent.ErrStalled if nothing real streams within d.
// firstByte disarms it for good; renew restarts the window (a step_start shows the model is working) until then.
func armFirstOutput(ctx context.Context, d time.Duration) (cctx context.Context, firstByte, renew, stop func()) {
	cctx, cc := context.WithCancelCause(ctx)
	timer := time.AfterFunc(d, func() { cc(agent.ErrStalled) })
	var disarmed atomic.Bool
	firstByte = func() { disarmed.Store(true); timer.Stop() }
	renew = func() {
		if !disarmed.Load() {
			timer.Reset(d)
		}
	}
	stop = func() { timer.Stop(); cc(nil) }
	return cctx, firstByte, renew, stop
}

// resolveModel: "" takes provider's opus alias; an alias name maps through provider's table; anything else (a routed
// cell's bare model, a raw id or "provider:model") is parsed against it directly.
func (l hubDispatch) resolveModel(reqModel string) string {
	reqModel = strings.TrimPrefix(reqModel, l.provider.OpencodeID+"/")
	var id string
	switch {
	case reqModel == "":
		id = l.provider.Models[models.Opus]
	case slices.Contains(models.AliasNames, reqModel):
		id = l.provider.Models[reqModel]
	default:
		id = providers.ParseTarget(l.reg, reqModel, l.provider).Model
	}
	if id == "auto" && l.provider.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
		id = l.provider.OpencodeFallback
	}
	return models.Strip1M(id)
}
