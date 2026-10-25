package agent

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// HubAgents are the agent names `ask` routes through the opencode hub by default; --native skips it. codex and
// copilot run their own CLI first (user decision): the hub cannot pass copilot's `auto` or its pinned account.
var HubAgents = []string{"opencode"}

// IsHubAgent reports whether name is one ask routes through opencode unless --native is set.
func IsHubAgent(name string) bool { return slices.Contains(HubAgents, name) }

// HubStart resolves the provider a hub agent's first leg targets: explicitProvider when the caller set
// one, else the registry's ladder[0]. The provider must carry an OpencodeID — the hub has no route to one that doesn't.
func HubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	name := explicitProvider
	if name == "" && len(reg.OpencodeLadder) > 0 {
		name = reg.OpencodeLadder[0]
	}
	if name == "" {
		return nil, fmt.Errorf("%s: no opencode ladder configured (set opencodeLadder with `claude-code vault edit`)", agentName)
	}
	p, err := reg.Get(name)
	if err != nil {
		return nil, err
	}
	if p.OpencodeID == "" {
		return nil, fmt.Errorf("%s: provider %s has no opencodeID, so the hub cannot route to it", agentName, p.Name)
	}
	return p, nil
}

// hubLeg is one opencode-routed ladder step: `opencode run -m <opencodeID>/<model>` on provider, under
// the exposed agent name — so Result.Agent reads the same across every leg of the chain, and only
// Result.Provider shows which account actually answered.
type hubLeg struct {
	agent    string
	provider *providers.Provider
	reg      *providers.Registry
}

// NewHubLeg builds a hub leg Runner for agentName on provider; reg resolves a provider:model override.
func NewHubLeg(agentName string, provider *providers.Provider, reg *providers.Registry) Runner {
	return hubLeg{agent: agentName, provider: provider, reg: reg}
}

func (l hubLeg) Name() string { return l.agent }

// Caps mirrors vendorRunner's: every Request field the opencode CLI can carry.
func (l hubLeg) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true, Autonomous: true}
}

// Available requires the opencode binary and an opencode login for this leg's provider; a login-less
// ladder step reports why here instead of failing mid-exec.
func (l hubLeg) Available() error {
	if _, err := paths.VendorBin("opencode"); err != nil {
		return err
	}
	_, err := vendors.OpencodeCredential(l.provider.OpencodeID)
	return err
}

func (l hubLeg) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	model := l.resolveModel(req.Model)
	res := Result{Provider: l.provider.Name, Model: model, SessionID: req.SessionID}
	if model == "" {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: fmt.Errorf("%s: no model resolved", l.provider.Name)}
	}
	if len(req.Prompt) > MaxPromptBytes {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: errPromptTooLarge(len(req.Prompt))}
	}
	bin, err := paths.VendorBin("opencode")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: err}
	}
	h := vendors.Headless{Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, WorkDir: req.WorkDir}
	cred, err := vendors.OpencodeCredential(l.provider.OpencodeID)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: err}
	}
	env, argv := vendors.OpencodeRunArgv(l.provider.OpencodeID+"/"+model, h)
	env = append(env, cred...)
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, "", req.KeepEnv, true)
	out, runErr := runEventStream(ctx, l.agent, cmd, tail, stream, parseOpenCodeStream)
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, runErr
}

// resolveModel: "" takes provider's opus slot; a slot name maps through provider's table; anything else (a routed
// cell's bare model, a raw id or "provider:model") is parsed against it directly.
func (l hubLeg) resolveModel(reqModel string) string {
	reqModel = strings.TrimPrefix(reqModel, l.provider.OpencodeID+"/")
	var id string
	switch {
	case reqModel == "":
		id = l.provider.Models[models.Opus]
	case slices.Contains(models.Slots, reqModel):
		id = l.provider.Models[reqModel]
	default:
		id = providers.ParseTarget(l.reg, reqModel, l.provider).Model
	}
	if id == "auto" && l.provider.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
		id = l.provider.OpencodeFallback
	}
	return models.Strip1M(id)
}
