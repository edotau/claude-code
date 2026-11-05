package copilot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/vault"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// copilotEngine is copilot's engine and its Engine: it drives the CLI through the SDK; tools are denied unless asked.
type copilotEngine struct{ agent.BaseEngine }

// copilotAgent is the one copilot engine, shared by the driver registry and the agent roster.
var copilotAgent = copilotEngine{agent.NewBaseEngine("copilot")}

var (
	_ agent.Driver        = copilotAgent
	_ agent.Engine        = copilotAgent
	_ agent.Authenticator = copilotEngine{}
)

func (e copilotEngine) Run(ctx context.Context, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	ctx = agent.DirectCtx(ctx, req)
	cs := copilotSchema()
	t, own, err := cs.TargetCtx(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}
	res, err := initialResult(cs, t, own)
	if err != nil {
		return res, err
	}
	opts, env, done, err := e.options(ctx, cs, req, t, own)
	defer done() // done is never nil, so an error path still releases a registry route
	if err != nil {
		return res, err
	}
	prompt, images, err := copilotPrompt(req)
	if err != nil {
		return res, copilotFatal(err)
	}
	out, err := copilotAsk(ctx, opts, prompt, images)
	lg := agent.EventLogFor(ctx)
	err = markToolRan(out, sink, err)
	if err != nil {
		lg.Emit("error", err.Error())
		if quotaHandoffEligible(own, t, err) {
			res.Usage = out.Usage // tokens the exhausted run spent still count
			return CopilotQuotaHandoff(req, res, out.SessionID, out.Text, err, CredentialValues(env, opts.GitHubToken))
		}
		return copilotPartial(res, out, err, sink)
	}
	lg.Emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	res.Model = cmp.Or(out.Model, res.Model)
	res.SessionID, res.Answer, res.Usage = out.SessionID, out.Text, out.Usage
	if strings.TrimSpace(out.Text) == "" {
		return res, agent.EmptyAnswer("copilot")
	}
	return res, sink.Emit(agent.Event{Type: agent.EventText, Text: out.Text})
}

// initialResult seeds the run result with the provider and model of target t.
func initialResult(cs agent.Schema, t providers.Target, own bool) (agent.Result, error) {
	res := agent.Result{Provider: cs.OwnProfile(), Model: models.Strip1M(t.Model)}
	if own {
		return res, nil
	}
	if t.Provider == nil {
		return res, copilotFatal(errors.New("agent target resolved to no provider"))
	}
	res.Provider = t.Provider.Name
	return res, nil
}

// quotaHandoffEligible reports whether err is a quota failure on a copilot-billed route, so the task can hand off.
func quotaHandoffEligible(own bool, t providers.Target, err error) bool {
	return (own || t.Provider.Auth.Login == "copilot") && agent.Classify("copilot", err, false).Kind == agent.KindQuota
}

// options builds the SDK options for target t; env is the BYOK route env; done (never nil) releases a registry route.
func (e copilotEngine) options(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target, own bool,
) (opts SDKOptions, env []string, done func(), err error) {
	done = func() {}
	route, env, err := e.routeEnv(ctx, cs, req, t, own)
	if err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	if route != nil {
		done = route.close
	}
	bin, err := e.Bin()
	if err != nil {
		return opts, nil, done, err
	}
	opts = newSDKOptions(bin, env, req, t, own)
	if route != nil {
		route.apply(&opts)
	}
	// the SDK child gets exactly BuildClientOptions' env; its base drops foreign credentials under ctx isolation
	if err := agent.CheckRouteNames(slices.Concat(opts.Env, opts.ChildEnv), req.OverlaySet); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	opts.Env = append(opts.Env, req.Env...) // --env goes last so it wins
	if err := applyCopilotToken(&opts, own, env); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	return opts, env, done, nil
}

// routeEnv prepares the registry route (own: none) or the BYOK route env for target t.
func (e copilotEngine) routeEnv(ctx context.Context, cs agent.Schema, req agent.RunOptions, t providers.Target, own bool,
) (route *copilotRegistryRoute, env []string, err error) {
	switch {
	case own:
	case copilotRegistryEligible(req.Direct, t):
		route, err = prepareCopilotRegistry(ctx, t)
	default:
		env, _, err = cs.Configure(ctx, t, "")
	}
	return route, env, err
}

// newSDKOptions builds the SDK options for one run from the CLI path, route env and request.
func newSDKOptions(bin string, env []string, req agent.RunOptions, t providers.Target, own bool) SDKOptions {
	var model string
	if own || (t.Provider != nil && t.Provider.Auth.Login != "") {
		model = copilotModelID(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	// An explicit --effort overrides the pin; there is no per-call --context, so the pin alone decides it.
	rec := vault.GetAgentRecord("copilot")
	workDir := req.WorkDir
	if workDir == "" {
		workDir, _ = os.Getwd() // an empty root would make every scoped write fail RelInside
	}
	o := SDKOptions{
		CLIPath: bin, Env: env, KeepEnv: req.KeepEnv, Model: model,
		ReasoningEffort: cmp.Or(req.Effort, rec.Effort),
		Context:         rec.Context, WorkingDirectory: workDir,
		AllowAllTools: req.Tools, ReadOnlyTools: req.ReadOnly, Permissions: req.Permissions, UseLoggedInUser: true, Autonomous: req.Autonomous, Fleet: req.Fleet, EnableMemory: req.EnableMemory,
		SessionID: req.SessionID,
		System:    strings.TrimSpace(req.SystemPrompt + "\n\n" + req.AppendSystemPrompt),
	}
	if home, err := paths.CopilotHome(); err == nil { // the dirs the harness renders under the copilot home
		o.InstructionDirs = existingDirs(home)
		o.SkillDirs = existingDirs(filepath.Join(home, "skills"))
		o.AgentDirs = existingDirs(filepath.Join(home, "agents"))
	}
	return o
}

// copilotPrompt inlines text attachments; the SDK never resolves @<path>, so images ride as absolute file paths.
func copilotPrompt(req agent.RunOptions) (prompt string, images []string, err error) {
	images, rest := agent.SplitImages(req.Attach)
	if images, err = absPaths(images); err != nil {
		return "", nil, err
	}
	prompt, err = agent.AttachInline(req.Prompt, rest)
	return prompt, images, err
}

// markToolRan marks progress when the SDK run approved a tool, so a run that acted is not replayed.
func markToolRan(out SDKResult, sink agent.Sink, err error) error {
	if out.ToolRan {
		err = errors.Join(err, sink.Emit(agent.Event{Type: agent.EventProgress}))
	}
	return err
}

// copilotPartial keeps a failed run's partial text and session; streamed bytes mark progress, so Run does not retry.
func copilotPartial(res agent.Result, out SDKResult, err error, sink agent.Sink) (agent.Result, error) {
	res.SessionID, res.Answer, res.Usage = cmp.Or(out.SessionID, res.SessionID), out.Text, out.Usage
	if strings.TrimSpace(out.Text) != "" {
		if serr := sink.Emit(agent.Event{Type: agent.EventText, Text: out.Text}); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return res, err
}

func copilotFatal(err error) error {
	return &agent.Error{Kind: agent.KindFatal, Agent: "copilot", Err: err}
}

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = SDKAsk

// applyCopilotToken puts the own-login launch token, then the harness account's env token, on the SDK's token field.
func applyCopilotToken(opts *SDKOptions, own bool, env []string) error {
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
		if tok, ok := strings.CutPrefix(kv, providers.CopilotTokenEnv+"="); ok && tok != "" {
			opts.GitHubToken, opts.UseLoggedInUser = tok, false
		}
	}
	return nil
}

// copilotModelID is id as Copilot's catalog spells it (dotted versions); [1m] is dropped, an unknown id is kept.
func copilotModelID(id string) string {
	id = models.Strip1M(id)
	if canonical, _, ok := models.Resolve(id); ok {
		return models.Wire("dotted", canonical)
	}
	return id
}

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

func (copilotEngine) Login(_ *providers.Provider, model string, h *agent.Headless) (env, argv []string, err error) {
	return copilotLogin(model, h)
}

func (copilotEngine) Wire(_ context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	return copilotWire(r.Provider, r.Dialect, r.Base, r.Model, providers.ContextWindow(r.Target.Provider, client), r.Cred, r.Routed, h)
}

func (e copilotEngine) FallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return agent.RouteFallback(reg, e.DescribeRoute(reg))
}

func (copilotEngine) DescribeRoute(reg *providers.Registry) agent.RouteInfo {
	return describeCopilotRoute(reg)
}

func (copilotEngine) Gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return copilotGate(reg, t)
}

// GateExplicit also turns a gate move off the pinned GitHub profile into a quota error and keeps the requested tier.
func (e copilotEngine) GateExplicit(req agent.RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	from := t.Provider
	t, note, err := copilotGate(reg, t)
	if err == nil && t.Provider != from && req.Provider != "" && req.Provider == vault.GetAgentRecord("copilot").Profile {
		return t, note, &agent.Error{Kind: agent.KindQuota, Agent: e.Name(), Err: errors.New(note)}
	}
	if tier := ResumeAlias(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
		t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
	}
	return t, note, err
}

func (e copilotEngine) NoRoute(reg *providers.Registry, _ providers.Target) error {
	return agent.RouteErr(e.DescribeRoute(reg))
}

// FallbackNote keeps the Databricks fallback's own notice: the gate already names the account and the profile.
func (copilotEngine) FallbackNote(ft providers.Target, note string) (string, bool) {
	return note, ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks
}

func (copilotEngine) Presence(router.Cell) error {
	if u := copilotPinLogin(); u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// Verify is copilot's config.json verdict (CopilotStoredLogin), not AuthStatus, so provider rows print as before.
func (copilotEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	if l := CopilotStoredLogin(); l != "" {
		return providers.VerdictOK, "signed in as " + l
	}
	return providers.VerdictNeedsLogin, ""
}

func (copilotEngine) Render(reg *providers.Registry, sel providers.Selection, home string, o render.RenderOptions, out io.Writer) (bool, error) {
	return renderCopilot(reg, sel, home, o, out)
}

func (copilotEngine) ConfigPaths(home string) ([]render.LabelPath, error) {
	home, err := render.HomeOr(home)
	if err != nil {
		return nil, err
	}
	return []render.LabelPath{
		{Label: "copilot-hooks", Path: filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")},
		{Label: "copilot-settings", Path: filepath.Join(home, ".copilot", "settings.json")},
	}, nil
}

func (copilotEngine) NativeRegistry(ctx context.Context, t providers.Target) bool {
	return UsesCopilotRegistry(ctx, t)
}

func (copilotEngine) RegistryLaunch(
	ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool,
) (agent.RegistryLaunch, error) {
	cr, err := ConfigureCopilotRegistry(ctx, reg, t, args, dry)
	if err != nil {
		return agent.RegistryLaunch{}, err
	}
	return agent.RegistryLaunch{Env: cr.Env, Unset: cr.Unset, Argv: cr.Argv, Args: cr.Args, Warnings: cr.Warnings,
		Files: cr.Files, Revoke: cr.Revoke, Banner: CopilotRegistryBanner}, nil
}

func (copilotEngine) RouteDetail() (string, bool) {
	reg, _, selErr, ok := agent.LoadSelection()
	if !ok {
		return "", false
	}
	return copilotRouteDetail(reg, selErr), true
}

// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, GH_TOKEN); blanked BYOK vars cannot redirect it.
func copilotLogin(model string, h *agent.Headless) ([]string, []string, error) {
	if model == "auto" { // no pin: blank (copilot reads "" as unset) so copilot's own /model default applies
		model = ""
	}
	env := []string{"COPILOT_MODEL=" + model, "COPILOT_PROVIDER_BASE_URL=", "COPILOT_OFFLINE=", "COPILOT_PROVIDERS_CONFIG="}
	tok, err := CopilotLaunchToken()
	if err != nil {
		return nil, nil, err
	}
	if tok != "" { // the harness's chosen account beats any inherited token
		env = append(env, providers.CopilotTokenEnv+"="+tok)
	}
	return env, copilotArgv(h), nil
}

// copilotArgv is the headless argv for h, else the configure page's pins on an interactive launch.
func copilotArgv(h *agent.Headless) []string {
	if h == nil {
		return copilotPinArgv()
	}
	return copilotHeadless(h)
}

// copilotPinArgv is --reasoning-effort/--context from the configure page's pins, on every interactive launch.
func copilotPinArgv() []string {
	var argv []string
	rec := vault.GetAgentRecord("copilot")
	if e := rec.Effort; e != "" {
		argv = append(argv, "--reasoning-effort", e)
	}
	if c := rec.Context; c != "" {
		argv = append(argv, "--context", c)
	}
	return argv
}

// copilotWire wires BYOK; window is the model's context window (providers.ContextWindow), which sizes the prompt cap.
func copilotWire(
	p *providers.Provider, dialect, base, model string, window int, cred string, routed bool, h *agent.Headless,
) ([]string, []string, error) {
	typ, base, wire, modelID, err := copilotEndpoint(p, dialect, base, model, routed)
	if err != nil {
		return nil, nil, err
	}
	window, output := copilotCaps(window, model)
	env := []string{
		"COPILOT_PROVIDER_BASE_URL=" + base,
		"COPILOT_PROVIDER_TYPE=" + typ,
		"COPILOT_MODEL=" + model,
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=" + strconv.Itoa(output),
		"COPILOT_OFFLINE=true",
		// Blank every BYOK credential var so a token exported in the parent shell cannot outrank this route.
		"COPILOT_PROVIDER_API_KEY=", "COPILOT_PROVIDER_BEARER_TOKEN=", "COPILOT_PROVIDER_API_KEY_COMMAND=",
	}
	if wire == providers.ProtocolResponses {
		env = append(env, "COPILOT_PROVIDER_WIRE_API=responses")
	}
	if modelID != "" {
		env = append(env, "COPILOT_PROVIDER_MODEL_ID="+modelID)
	}
	// Prompt budget = window minus the output cap and a 10% margin, so copilot compacts before the endpoint refuses.
	if n := copilotPromptTokens(window, output); n > 0 {
		env = append(env, "COPILOT_PROVIDER_MAX_PROMPT_TOKENS="+strconv.Itoa(n))
	}
	return append(env, copilotCredEnv(p, cred)...), copilotArgv(h), nil
}

// copilotEndpoint picks the BYOK type and base; a non-Claude model on an anthropic dialect takes its openai|responses wire.
func copilotEndpoint(p *providers.Provider, dialect, base, model string, routed bool) (typ, b, wire, modelID string, err error) {
	if dialect != providers.ProtocolAnthropic {
		return "openai", base, "", "", nil
	}
	if models.FamilyKind(model) == models.FamilyClaude {
		return "anthropic", base, "", "", nil
	}
	w, canonical, ok, why := copilotEndpointWire(p, model)
	if !ok {
		return "", "", "", "", fmt.Errorf("copilot: %s", why)
	}
	switch {
	case w == providers.ProtocolAnthropic:
		return "anthropic", base, "", "", nil
	case routed: // the router's openai|responses passthrough
		return "openai", base + "/" + w, w, canonical, nil
	}
	if b, ok = p.Route(w); !ok { // --direct: the profile's own endpoint for that wire
		return "", "", "", "", fmt.Errorf("copilot: profile %s has no %s route for %s", p.Name, w, model)
	}
	return "openai", b, w, "", nil
}

// copilotCredEnv is the BYOK credential and the provider's extra headers.
func copilotCredEnv(p *providers.Provider, cred string) []string {
	var env []string
	if cred != "" {
		// Bearer precedence is copilot's own rule; sending the key on both headers trips strict gateways.
		if p.Auth.HeaderStyle() == providers.HeaderBearer {
			env = append(env, "COPILOT_PROVIDER_BEARER_TOKEN="+cred)
		} else {
			env = append(env, "COPILOT_PROVIDER_API_KEY="+cred)
		}
	}
	if len(p.Headers) > 0 {
		var hs []string
		for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
			hs = append(hs, k+": "+p.Headers[k])
		}
		env = append(env, "COPILOT_PROVIDER_HEADERS="+strings.Join(hs, "\n"))
	}
	return env
}

func copilotHeadless(h *agent.Headless) []string {
	argv := []string{"--prompt", h.Prompt}
	switch {
	case h.Tools && h.Autonomous:
		argv = append(argv, "--allow-all-tools")
	}
	return argv
}

// CopilotCatalog is where `copilot models` lists from: a Databricks row's endpoints, or GitHub's own catalog.
type CopilotCatalog struct {
	Source     string
	Profile    string
	Databricks bool
	Rows       []CopilotEndpoint
}

// Write renders a Databricks row's chat endpoints as a NAME / TESTED table, or JSON.
func (c CopilotCatalog) Write(w io.Writer, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(w, c)
	}
	fmt.Fprintln(w, c.Source)
	nameW := len("NAME")
	for _, r := range c.Rows {
		nameW = max(nameW, len(r.ID))
	}
	fmt.Fprintf(w, "%-*s  %s\n", nameW, "NAME", "TESTED")
	for _, r := range c.Rows {
		mark := ""
		if r.Tested {
			mark = "tested"
		}
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("%-*s  %s", nameW, r.ID, mark), " "))
	}
	return nil
}

// CopilotModelSource resolves profile (default: the session profile) and lists its chat endpoints if Databricks.
func CopilotModelSource(ctx context.Context, reg *providers.Registry, profile string) (CopilotCatalog, error) {
	name := profile
	if name == "" {
		name = providers.SessionProfile()
	}
	p, ok := reg.Providers[name]
	if !ok || p == nil || p.Preset != providers.PresetDatabricks {
		return CopilotCatalog{Source: "GitHub Copilot catalog"}, nil
	}
	rows, err := checkedCopilotEndpoints(ctx, reg, p)
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true, Rows: rows}, nil
}

// copilotSchema is the registered copilot roster row.
func copilotSchema() agent.Schema {
	s, _ := agent.LookupSchema("copilot")
	return s
}
