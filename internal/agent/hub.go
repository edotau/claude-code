package agent

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// HubAgents are the exposed agent names `ask` routes through the opencode hub by default; --native
// (or another agent name) keeps the vendor's own CLI.
var HubAgents = []string{"codex", "copilot", "opencode"}

// IsHubAgent reports whether name is one ask routes through opencode unless --native is set.
func IsHubAgent(name string) bool { return slices.Contains(HubAgents, name) }

// hubStart fixes each agent's first ladder step; "opencode" has none here and falls to the registry's
// own ladder[0] in HubStart.
var hubStart = map[string]string{"codex": "chatgpt", "copilot": "github-copilot"}

// HubStart resolves the provider a hub agent's first leg targets: explicitProvider when the caller set
// one, else the agent's fixed start, else (opencode) the registry's ladder[0]. The provider must carry
// an OpencodeID — the hub has no route to one that doesn't.
func HubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	name := explicitProvider
	if name == "" {
		name = hubStart[agentName]
	}
	if name == "" && len(reg.OpencodeLadder) > 0 {
		name = reg.OpencodeLadder[0]
	}
	if name == "" {
		return nil, fmt.Errorf("%s: no opencode ladder configured (set opencodeLadder in providers.json)", agentName)
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

// HubChain is the rest of a hub agent's fallback chain after start: the registry ladder rotated to
// begin right after start, then claude under FallbackAll. A ladder entry opencode has no login for
// still rides the chain — its Available() reports that, and RunWithFallback/legUnavailable skip it
// the same way an unreachable vendor CLI is skipped today.
func HubChain(reg *providers.Registry, agentName string, start *providers.Provider, mode string) []Runner {
	if mode == FallbackNone {
		return nil
	}
	var chain []Runner
	for _, name := range rotateLadder(reg.OpencodeLadder, start.Name) {
		if name == start.Name {
			continue
		}
		if p, ok := reg.Providers[name]; ok && p.OpencodeID != "" {
			chain = append(chain, hubLeg{agent: agentName, provider: p, reg: reg})
		}
	}
	if mode == FallbackAll {
		chain = append(chain, claudeRunner{})
	}
	return chain
}

// rotateLadder returns ladder starting at start's first occurrence, wrapping around; start absent
// from ladder returns ladder unchanged.
func rotateLadder(ladder []string, start string) []string {
	i := slices.Index(ladder, start)
	if i < 0 {
		return ladder
	}
	return append(slices.Clone(ladder[i:]), ladder[:i]...)
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
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true}
}

// Available requires the opencode binary and an opencode login for this leg's provider; a login-less
// ladder step reports why here instead of failing mid-exec.
func (l hubLeg) Available() error {
	if _, err := paths.LookPathReal("opencode"); err != nil {
		return err
	}
	if !slices.Contains(vendors.OpencodeLogins(), l.provider.OpencodeID) {
		return fmt.Errorf("opencode has no %s login (run `opencode auth login`)", l.provider.Name)
	}
	return nil
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
	bin, err := paths.LookPathReal("opencode")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: err}
	}
	h := vendors.Headless{Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools}
	env, argv := vendors.OpencodeRunArgv(l.provider.OpencodeID+"/"+model, h)
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, "")
	out, runErr := runEventStream(ctx, l.agent, cmd, tail, stream, parseOpenCodeStream)
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, runErr
}

// resolveModel: "" takes provider's opus slot (every vendor runner's own default today); a slot name
// maps through provider's table; anything else (a raw id or "provider:model") is parsed against it
// directly — the raw-id case only ever reaches the first leg, since fallbackRequest resets Model to ""
// for every leg after that.
func (l hubLeg) resolveModel(reqModel string) string {
	switch {
	case reqModel == "":
		return models.Strip1M(l.provider.Models[models.Opus])
	case slices.Contains(models.Slots, reqModel):
		return models.Strip1M(l.provider.Models[reqModel])
	default:
		return models.Strip1M(providers.ParseTarget(l.reg, reqModel, l.provider).Model)
	}
}
