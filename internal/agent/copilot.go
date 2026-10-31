package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// copilotEngine is copilot's engine and its Runner: it drives the CLI through the SDK; tools are denied unless asked.
type copilotEngine struct{ baseEngine }

// copilotAgent is the one copilot engine, shared by the engines table and the runner roster.
var copilotAgent = copilotEngine{baseEngine{"copilot"}}

var (
	_ engine = copilotAgent
	_ Runner = copilotAgent
)

func (e copilotEngine) Run(ctx context.Context, req RunOptions, sink Sink) (Result, error) {
	ctx = directCtx(ctx, req)
	cs := schemaFor(e.name)
	t, own, err := cs.targetCtx(ctx, req)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: cs.ownProfile(), Model: models.Strip1M(t.Model)}
	if !own {
		if t.Provider == nil {
			return res, copilotFatal(errors.New("agent target resolved to no provider"))
		}
		res.Provider = t.Provider.Name
	}
	opts, env, done, err := e.options(ctx, cs, req, t, own)
	if err != nil {
		return res, err
	}
	defer done()
	prompt, images, err := copilotPrompt(req)
	if err != nil {
		return res, copilotFatal(err)
	}
	out, err := copilotAsk(ctx, opts, prompt, images)
	lg := eventLogFor(ctx)
	if err != nil {
		lg.emit("error", err.Error())
		if (own || t.Provider.Auth.Login == "copilot") && Classify("copilot", err, false).Kind == KindQuota {
			return copilotQuotaHandoff(req, res, out, err, credentialValues(env, opts.GitHubToken))
		}
		return copilotPartial(res, out, err, sink)
	}
	lg.emit("assistant", out.Text) // the SDK returns the whole text, so one line per run
	res.Model = cmp.Or(out.Model, res.Model)
	res.SessionID, res.Answer = out.SessionID, out.Text
	if strings.TrimSpace(out.Text) == "" {
		return res, emptyAnswer("copilot")
	}
	return res, sink.Emit(Event{Type: EventText, Text: out.Text})
}

// options builds the SDK options for target t; env is the BYOK route env and done releases a registry route.
func (e copilotEngine) options(ctx context.Context, cs Schema, req RunOptions, t providers.Target, own bool,
) (opts copilotsdk.Options, env []string, done func(), err error) {
	done = func() {}
	var route *copilotRegistryRoute
	switch {
	case own:
	case copilotRegistryEligible(req.Direct, t):
		if route, err = prepareCopilotRegistry(ctx, t); err != nil {
			return opts, nil, done, copilotFatal(err)
		}
		done = route.close
	default:
		if env, _, err = cs.Configure(ctx, t, ""); err != nil {
			return opts, nil, done, copilotFatal(err)
		}
	}
	bin, err := e.bin()
	if err != nil {
		return opts, nil, done, err
	}
	var model string
	if own || (t.Provider != nil && t.Provider.Auth.Login != "") {
		model = copilotModelID(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	// An explicit --effort overrides the pin; there is no per-call --context, so the pin alone decides it.
	opts = copilotsdk.Options{
		CLIPath: bin, Env: env, KeepEnv: req.KeepEnv, Model: model,
		ReasoningEffort: cmp.Or(req.Effort, providers.Pin(providers.PinCopilotEffort)),
		Context:         providers.Pin(providers.PinCopilotContext), WorkingDirectory: req.WorkDir,
		AllowAllTools: req.Tools, UseLoggedInUser: true, Autonomous: req.Autonomous, EnableMemory: req.EnableMemory,
		SessionID: req.SessionID,
	}
	if route != nil {
		route.apply(&opts)
	}
	// the SDK merges os.Environ itself, so blank other profiles' credentials the launch does not set itself
	opts.Env = append(blankForeignCredentials(ctx, opts.Env, req.KeepEnv), req.Env...) // --env goes last so it wins
	if err := applyCopilotToken(&opts, own, env); err != nil {
		return opts, nil, done, copilotFatal(err)
	}
	return opts, env, done, nil
}

// copilotPrompt inlines text attachments; the SDK never resolves @<path>, so images ride as absolute file paths.
func copilotPrompt(req RunOptions) (prompt string, images []string, err error) {
	images, rest := splitImages(req.Attach)
	if images, err = absPaths(images); err != nil {
		return "", nil, err
	}
	prompt, err = attachInline(req.Prompt, rest)
	return prompt, images, err
}

// copilotPartial keeps a failed run's partial text and session; streamed bytes mark progress, so Run does not retry.
func copilotPartial(res Result, out copilotsdk.Result, err error, sink Sink) (Result, error) {
	res.SessionID, res.Answer = cmp.Or(out.SessionID, res.SessionID), out.Text
	if strings.TrimSpace(out.Text) != "" {
		if serr := sink.Emit(Event{Type: EventText, Text: out.Text}); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	return res, err
}

func copilotFatal(err error) error { return &Error{Kind: KindFatal, Agent: "copilot", Err: err} }

// copilotAsk is the SDK call, a var so tests reach the agent's result handling without a copilot binary.
var copilotAsk = copilotsdk.Ask

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

func (copilotEngine) login(_ *providers.Provider, model string, h *Headless) (env, argv []string, err error) {
	return copilotLogin(model, h)
}

func (copilotEngine) wire(_ context.Context, r wireRoute, h *Headless) (env, argv []string, err error) {
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	return copilotWire(r.Provider, r.Dialect, r.Base, r.Model, providers.ContextWindow(r.Target.Provider, client), r.Cred, r.Routed, h)
}

func (e copilotEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return routeFallback(reg, e.describeRoute(reg))
}

func (copilotEngine) describeRoute(reg *providers.Registry) RouteInfo {
	return describeCopilotRoute(reg)
}

func (copilotEngine) LoginStatus() (bool, string) {
	if a, ok := ResolveCopilotAuth(); ok {
		return true, a.Label()
	}
	return false, ""
}

func (copilotEngine) gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return copilotGate(reg, t)
}

// gateExplicit also turns a gate move off the pinned GitHub profile into a quota error and keeps the requested tier.
func (e copilotEngine) gateExplicit(req RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	from := t.Provider
	t, note, err := copilotGate(reg, t)
	if err == nil && t.Provider != from && req.Provider != "" && req.Provider == providers.Pin(providers.PinCopilotProfile) {
		return t, note, &Error{Kind: KindQuota, Agent: e.name, Err: errors.New(note)}
	}
	if tier := resumeSlot(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
		t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
	}
	return t, note, err
}

func (e copilotEngine) noRoute(reg *providers.Registry, _ providers.Target) error {
	return routeErr(e.describeRoute(reg))
}

// fallbackNote keeps the Databricks fallback's own notice: the gate already names the account and the profile.
func (copilotEngine) fallbackNote(ft providers.Target, note string) (string, bool) {
	return note, ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks
}

func (copilotEngine) presence(Cell) error {
	if u := copilotPinLogin(); u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// loginPresent stays local to copilot: LoginStatus's label may call api.github.com.
func (copilotEngine) loginPresent() bool { return CopilotLoginPresent() }

func (copilotEngine) checkLogin(context.Context) (providers.AuthVerdict, string) {
	if l := signinCopilotLogin(); l != "" {
		return providers.VerdictOK, "signed in as " + l
	}
	return providers.VerdictNeedsLogin, ""
}

func (copilotEngine) render(reg *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error) {
	return renderCopilot(reg, sel, home, o, out)
}

func (copilotEngine) configPaths(home string) ([]labelPath, error) {
	home, err := homeOr(home)
	if err != nil {
		return nil, err
	}
	return []labelPath{
		{"copilot-hooks", filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")},
		{"copilot-settings", filepath.Join(home, ".copilot", "settings.json")},
	}, nil
}

func (copilotEngine) nativeRegistry(ctx context.Context, t providers.Target) bool {
	return UsesCopilotRegistry(ctx, t)
}

func (copilotEngine) routeDetail() (string, bool) {
	reg, _, selErr, ok := loadSelection()
	if !ok {
		return "", false
	}
	return copilotRouteDetail(reg, selErr), true
}

// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, GH_TOKEN); blanked BYOK vars cannot redirect it.
func copilotLogin(model string, h *Headless) ([]string, []string, error) {
	if model == "auto" { // no pin: blank (copilot reads "" as unset) so copilot's own /model default applies
		model = ""
	}
	env := []string{"COPILOT_MODEL=" + model, "COPILOT_PROVIDER_BASE_URL=", "COPILOT_OFFLINE=", "COPILOT_PROVIDERS_CONFIG="}
	tok, err := CopilotLaunchToken()
	if err != nil {
		return nil, nil, err
	}
	if tok != "" { // the harness's chosen account beats any inherited token
		env = append(env, CopilotTokenEnv+"="+tok)
	}
	return env, copilotArgv(h), nil
}

// copilotArgv is the headless argv for h, else the configure page's pins on an interactive launch.
func copilotArgv(h *Headless) []string {
	if h == nil {
		return copilotPinArgv()
	}
	return copilotHeadless(h)
}

// copilotPinArgv is --reasoning-effort/--context from the configure page's pins, on every interactive launch.
func copilotPinArgv() []string {
	var argv []string
	if e := providers.Pin(providers.PinCopilotEffort); e != "" {
		argv = append(argv, "--reasoning-effort", e)
	}
	if c := providers.Pin(providers.PinCopilotContext); c != "" {
		argv = append(argv, "--context", c)
	}
	return argv
}

// copilotWire wires BYOK; window is the model's context window (providers.ContextWindow), which sizes the prompt cap.
func copilotWire(
	p *providers.Provider, dialect, base, model string, window int, cred string, routed bool, h *Headless,
) ([]string, []string, error) {
	typ := "openai"
	if dialect == providers.ProtocolAnthropic {
		typ = "anthropic"
	}
	wire, modelID := "", ""
	if routed && dialect == providers.ProtocolAnthropic && models.FamilyKind(model) != models.FamilyClaude {
		w, canonical, ok, why := copilotEndpointWire(p, model)
		if !ok {
			return nil, nil, fmt.Errorf("copilot: %s", why)
		}
		if w != providers.ProtocolAnthropic { // the router's openai|responses passthrough
			wire, modelID = w, canonical
			typ, base = "openai", base+"/"+wire
		}
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

func copilotHeadless(h *Headless) []string {
	argv := []string{"--prompt", h.Prompt}
	if h.Tools {
		argv = append(argv, "--allow-all-tools")
	}
	return argv
}

// CopilotLoginPresent reports a usable GitHub login: a token env var, the keyring token, copilot's own, or gh's.
func CopilotLoginPresent() bool {
	_, ok := copilotAuthSource()
	return ok
}

// CopilotCatalog is where `copilot models` lists from: a Databricks row's endpoints, or GitHub's own catalog.
type CopilotCatalog struct {
	Source     string
	Profile    string
	Databricks bool
	Rows       []CopilotEndpoint
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
	rows, err := discoverCopilotEndpoints(ctx, reg, p)
	if err != nil {
		return CopilotCatalog{}, err
	}
	return CopilotCatalog{Source: name + " serving endpoints", Profile: name, Databricks: true, Rows: rows}, nil
}
