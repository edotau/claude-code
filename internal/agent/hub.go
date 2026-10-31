package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeLogins is a seam over OpencodeLogins, so tests never shell out to opencode auth.
var opencodeLogins = OpencodeLogins

// freeFirstOutput overrides the first-output deadline for a free-billed hub leg (route.BillFree); zero (the
// default) defers to the registry's routing.opencode.freeFirstOutput (providers.OpencodeRouting.FirstOutput,
// 90s unless the vault overlay sets it). A paid leg carries no deadline (hubLeg.firstOutput stays 0) and
// relies on AttemptTimeout alone.
var freeFirstOutput time.Duration

// firstOutputDeadline resolves the free-leg stall deadline: the freeFirstOutput override when tests set
// one, else the registry's own value (a load error falls back to providers.OpencodeRouting's 90s floor).
func firstOutputDeadline() time.Duration {
	if freeFirstOutput > 0 {
		return freeFirstOutput
	}
	reg, err := providers.Load()
	if err != nil {
		return (providers.OpencodeRouting{}).FirstOutput()
	}
	return reg.Routing.Opencode.FirstOutput()
}

// errStalled is the sentinel a free leg's Run returns when nothing streamed within firstOutput: a fatal,
// unwrapped failure, so Do never retries it but legUnavailable falls through to the chain's backup leg.
var errStalled = errors.New("free leg stalled: no output within the first-output deadline")

// IsHubAgent reports whether name is one ask routes through opencode unless --native is set. codex and
// copilot run their own CLI first (user decision): the hub cannot pass copilot's `auto` or its pinned account.
func IsHubAgent(name string) bool {
	return slices.ContainsFunc(rosterRows, func(s Schema) bool { return s.Name == name && s.CLI.Hub })
}

// HubStart resolves the provider a hub agent's first leg targets: explicitProvider when the caller set
// one (forced, regardless of login or quota), else the first opencodeLadder entry that has a login,
// falling back to the ladder's first entry at all so the page stays editable with no login anywhere.
// The ladder orders opencode's own paid backends only — the free Zen catalog (opus excluded, tier- and
// work-repo-gated) is the per-tier sub-pick's concern (route.Table.Pick), not this cross-tier provider page.
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
	for _, id := range reg.Routing.Opencode.LadderOrder() {
		p, err := opencodeLadderProvider(reg, id)
		if err != nil {
			continue
		}
		if first == nil {
			first = p
		}
		if slices.Contains(opencodeLogins(), OpencodeCredID(p.OpencodeID)) {
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

// hubLeg is one opencode-routed ladder step: `opencode run -m <opencodeID>/<model>` on provider, under
// the exposed agent name — so Result.Agent reads the same across every leg of the chain, and only
// Result.Provider shows which account actually answered.
type hubLeg struct {
	agent    string
	provider *providers.Provider
	reg      *providers.Registry
	// firstOutput, when non-zero, fails the leg if nothing streams within it — the free Zen leg's stall guard.
	firstOutput time.Duration
}

// NewHubLeg builds a hub leg Agent for agentName on provider; reg resolves a provider:model override.
func NewHubLeg(agentName string, provider *providers.Provider, reg *providers.Registry) Runner {
	return hubLeg{agent: agentName, provider: provider, reg: reg}
}

func (l hubLeg) Name() string { return l.agent }

// Caps mirrors cliRunner's: every RunOptions field the opencode CLI can carry.
func (l hubLeg) Caps() Caps {
	return Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native, Tools: Native, Autonomous: Native,
		SafeMode: Native, Direct: Native, Env: Native, SystemPrompt: Native, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated}
}

// Available requires the opencode binary and an opencode login for this leg's provider; a login-less
// ladder step reports why here instead of failing mid-exec.
func (l hubLeg) Available() error {
	if _, err := paths.VendorBin("opencode"); err != nil {
		return err
	}
	_, err := opencodeCredential(l.provider.OpencodeID)
	return err
}

func (l hubLeg) Run(ctx context.Context, req RunOptions, sink Sink) (Result, error) {
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
	h := Headless{Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Autonomous: req.Autonomous, Attach: req.Attach, WorkDir: req.WorkDir, SafeMode: req.SafeMode}
	cred, err := opencodeCredential(l.provider.OpencodeID)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: err}
	}
	env, argv := opencodeRunArgv(l.provider.OpencodeID+"/"+model, h)
	env = append(env, cred...)
	if req.SystemPrompt != "" {
		env, argv = opencodeSystemPrompt(env, argv, req.SystemPrompt)
	}
	env = append(env, req.Env...) // --env goes last so it wins
	cctx := ctx
	var onFirstByte func()
	var cancel context.CancelFunc
	if l.firstOutput > 0 {
		var cc context.CancelCauseFunc
		cctx, cc = context.WithCancelCause(ctx)
		cancel = func() { cc(nil) }
		timer := time.AfterFunc(l.firstOutput, func() { cc(errStalled) })
		onFirstByte = func() { timer.Stop() }
		defer timer.Stop()
		defer cancel()
	}
	cmd, tail := childCmd(cctx, bin, argv, env, req.WorkDir, "", req.KeepEnv, true)
	out, runErr := runEventStream(cctx, l.agent, cmd, tail, sink, parseOpenCodeStream, onFirstByte)
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	if l.firstOutput > 0 && errors.Is(context.Cause(cctx), errStalled) {
		return res, &Error{Kind: KindFatal, Agent: l.agent, Err: errStalled}
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
