package opencode

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"os/exec"
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
var opencodeLogins = opencodeLoginsErr

// HubStart picks explicitProvider, else the first opencodeLadder entry with a login, else the first entry.
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
	// probed once, on the first ladder entry that resolves
	var logins []string
	var probeErr error
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
			logins, probeErr = opencodeLogins()
			probed = true
		}
		if slices.Contains(logins, OpencodeCredID(p.OpencodeID)) {
			return p, nil
		}
	}
	if first != nil {
		// a failed probe is not "logged out"
		if len(logins) == 0 && probeErr != nil {
			return nil, fmt.Errorf("%s: opencode logins could not be read: %w", agentName, probeErr)
		}
		return first, nil
	}
	return nil, fmt.Errorf("%s: no opencode ladder configured (set opencode.ladder in `claude-code configure routing`)", agentName)
}

// opencodeLadderProvider resolves matching registry OpencodeIDs or a synthetic opencode-go provider.
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

// hubDispatch runs opencode on one ladder provider; Result.Agent stays fixed while Result.Provider names the account.
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

// Caps is the opencode schema row's capability set, so the hub engine cannot drift from it.
func (l hubDispatch) Caps() agent.Caps { return row.CLI.Caps }

// Available requires the binary and this provider's login, reporting missing logins before dispatch.
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
	bin, env, argv, err := l.prepareLaunch(ctx, req, model)
	if err != nil {
		return res, retry.Fatal(l.agent, err)
	}
	cmd, tail, host, err := opencodeChild(ctx, l.agent, bin, env, argv, req.Prompt, req)
	if err != nil {
		return res, err
	}
	w, opts, parse := l.idleWatch(cmd, host)
	defer w.Stop()
	out, runErr := agent.RunEventStream(ctx, l.agent, cmd, tail, sink, parse, opts)
	defer func() { host.after(out, runErr) }()
	res.Answer, res.Usage, res.Transport = out.Answer, out.Usage, out.Transport
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	// a stall, then a tools-off answer that is only tool calls
	if runErr != nil {
		return res, runErr
	}
	return res, refuseToolCallText(l.agent, req, res.Answer)
}

// idleWatch arms the free dispatch's first-output watch (step_start renews it); after first output the idle watch takes over.
func (l hubDispatch) idleWatch(cmd *exec.Cmd, host *hookHost) (*agent.IdleTimeout, agent.StreamOpts, hubParse) {
	limit := agent.IdleLimitFor(l.agent)
	w := agent.NewIdleTimeout(l.agent, cmp.Or(l.firstOutput, limit), nil)
	if l.firstOutput == 0 {
		w.Pause() // paid dispatch: no first-output bound, only the idle watch once the model spoke
	}
	var onStep func()
	if w != nil {
		onStep = func() { w.Beat("step_start") }
	}
	lw := watchLimit(cmd)
	spoke := false // read and set by the parser goroutine only
	onSpeak := func() {
		spoke = true
		w.Spoke()
		if limit > 0 {
			w.Rearm(limit, "first output")
		} else {
			w.Stop()
		}
		lw.markSpoke()
	}
	return w, agent.StreamOpts{Idle: w, OnMeaningful: onSpeak, Cause: lw.Err}, idleParser(host.parser(onStep), w, &spoke)
}

type hubParse = func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error)

// beatReader re-arms w on every read once the model has spoken, so a quiet stream is the only thing that trips it.
type beatReader struct {
	r     io.Reader
	w     *agent.IdleTimeout
	spoke *bool
}

func (b beatReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 && *b.spoke {
		b.w.Beat("event")
	}
	return n, err
}

// idleParser wraps parse so each chunk of the child's stdout re-arms w after first output.
func idleParser(parse hubParse, w *agent.IdleTimeout, spoke *bool) hubParse {
	return func(br *bufio.Reader, sink wire.Sink, onMeaningful func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parse(bufio.NewReader(beatReader{r: br, w: w, spoke: spoke}), sink, onMeaningful, logs...)
	}
}

func (l hubDispatch) prepareLaunch(ctx context.Context, req agent.RunOptions, model string) (string, []string, []string, error) {
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
	// headless: a refused stored record fails the run, it never falls back to the built-ins
	if err != nil {
		return "", nil, nil, err
	}
	// never errs
	env, argv, _, _, _ = agent.NewBaseEngine("opencode").RenderSettings(ctx, st, env, argv)
	env = append(env, cred...)
	if req.SystemPrompt != "" {
		env, argv = opencodeSystemPrompt(env, argv, req.SystemPrompt)
	}
	if err := agent.CheckRouteNames(env, req.OverlaySet, req.OverlayUnset); err != nil {
		return "", nil, nil, err
	}
	argv = opencodePolicyArgv(argv, req)
	// --env goes last so it wins
	env = append(env, req.Env...)
	return bin, env, argv, nil
}

// resolveModel uses opus for empty input, maps aliases via the provider, and parses raw IDs directly.
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
