package opencode

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeLogins is a seam over listOpencodeLogins, so tests never shell out to opencode auth.
var opencodeLogins = listOpencodeLogins

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

// newHubDispatch builds a hub dispatch Agent for agentName on provider; reg resolves a provider:model override.
func newHubDispatch(agentName string, provider *providers.Provider, reg *providers.Registry) agent.HubEngine {
	return hubDispatch{agent: agentName, provider: provider, reg: reg}
}

func (l hubDispatch) Name() string { return l.agent }

// HubProvider is the account this step dispatches on.
func (l hubDispatch) HubProvider() *providers.Provider { return l.provider }

// WithFirstOutput sets the free dispatch's idle timeout.
func (l hubDispatch) WithFirstOutput(d time.Duration) agent.Engine {
	l.firstOutput = d
	return l
}

// Caps mirrors cliEngine's: every RunOptions field the opencode CLI can carry.
func (l hubDispatch) Caps() agent.Caps {
	return agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native, Tools: agent.Native, Autonomous: agent.Native,
		ReadOnly: agent.Emulated, SafeMode: agent.Native, Permissions: agent.Native, Direct: agent.Native, Env: agent.Native, SystemPrompt: agent.Native, AppendSystemPrompt: agent.Emulated, OutputSchema: agent.Emulated, Persona: agent.Emulated}
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

func (l hubDispatch) Run(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	model := l.resolveModel(req.Model)
	res := wire.Result{Provider: l.provider.Name, Model: model, SessionID: req.SessionID}
	if model == "" {
		return res, retry.Fatal(l.agent, fmt.Errorf("%s: no model resolved", l.provider.Name))
	}
	if len(req.Prompt) > agent.MaxPromptBytes {
		return res, retry.Fatal(l.agent, agent.PromptTooLarge(len(req.Prompt), agent.MaxPromptBytes))
	}
	bin, env, argv, err := l.prepareLaunch(req, model)
	if err != nil {
		return res, retry.Fatal(l.agent, err)
	}
	cmd, tail, host, err := opencodeChild(ctx, l.agent, bin, env, argv, req.Prompt, req)
	if err != nil {
		return res, err
	}
	// the first-output watch: step_start renews it, the first real event stops it for good; RunEventStream kills.
	w := agent.NewIdleTimeout(l.agent, l.firstOutput, nil)
	defer w.Stop()
	var onStep func()
	if w != nil {
		onStep = func() { w.Beat("step_start") }
	}
	opts := agent.StreamOpts{Idle: w, OnMeaningful: w.Stop}
	out, runErr := agent.RunEventStream(ctx, l.agent, cmd, tail, sink, host.parser(onStep), opts)
	defer func() { host.after(res) }()
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	if runErr != nil { // a stall, then a tools-off answer that is only tool calls
		return res, runErr
	}
	return res, refuseToolCallText(l.agent, req, res.Answer)
}

func (l hubDispatch) prepareLaunch(req agent.RunOptions, model string) (string, []string, []string, error) {
	if err := refusePolicyEnv(req); err != nil {
		return "", nil, nil, err
	}
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
	env, argv := opencodeRun(l.provider.OpencodeID+"/"+model, h)
	st, err := agent.ResolveSettings("opencode", req, providers.Target{}, true)
	if err != nil { // headless: a refused stored record fails the run, it never falls back to the built-ins
		return "", nil, nil, err
	}
	env, argv, _, _, _ = agent.NewBaseEngine("opencode").RenderSettings(context.Background(), st, env, argv) // never errs
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
	return models.Strip1M(opencodeModel(l.provider, id))
}
