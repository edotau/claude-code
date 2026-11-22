package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotGate keeps t unless the GitHub login is unworkable, then the pin or Databricks, never another account.
func copilotGate(ctx context.Context, reg *providers.Registry, t providers.Target, explicit bool, mode string,
) (providers.Target, string, error) {
	if t.Provider == nil || t.Provider.Auth.Login != "copilot" {
		return t, "", nil
	}
	// the target GitHub row's own login: an explicit --profile <login> is gated on that account, not the copilot pin
	detail, ok := pinnedHeadroom(ctx, t.Provider.Login)
	if ok {
		return t, "", nil
	}
	// the route's chain picks the next hop; a silent Databricks move would leak
	if arm := router.ArmProfileContext(ctx); arm != "" {
		return providers.Target{}, "", fmt.Errorf("copilot: %s; GitHub hop refused inside databricks:%s", detail, arm)
	}
	mode = cmp.Or(mode, fallbackMode(ctx))
	if explicit && mode == router.FallbackNone {
		return providers.Target{}, "", fmt.Errorf("copilot: %s; the %s account was named explicitly", detail, t.Provider.Name)
	}
	if pt, ok := pinHop(reg, t); explicit && ok {
		if got, _, err := copilotGate(ctx, reg, pt, false, mode); err == nil && got.Provider == pt.Provider {
			return pt, fmt.Sprintf("copilot: %s; %s has no credits, using pin %s", detail, t.Provider.Name, pt.Provider.Name), nil
		}
		if mode == router.FallbackPin {
			return providers.Target{}, "", fmt.Errorf("copilot: %s; neither %s nor the pin %s can run", detail, t.Provider.Name, pt.Provider.Name)
		}
	}
	ri := copilotDatabricksFallback(ctx, reg, detail)
	ft, ok := agent.RouteTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	if strings.HasPrefix(t.Model, models.DatabricksPrefix) {
		if _, why := agent.ProfileServes("copilot", ft.Provider, t.Model); why != "" {
			return providers.Target{}, "", fmt.Errorf("copilot: %s; databricks profile %s %s", detail, ft.Provider.Name, why)
		}
	}
	return ft, ri.Reason, nil
}

// fallbackMode is the run's AGENT_ERROR_FALLBACK mode (process, run values, then vault env).
func fallbackMode(ctx context.Context) string {
	if v := strings.TrimSpace(proc.Value(ctx, router.EnvFallback)); v != "" {
		return router.ParseFallback(v)
	}
	return router.DefaultFallback()
}

// pinHop is copilot's record profile as a target other than t's row; false when the pin is t, unset or unusable.
func pinHop(reg *providers.Registry, t providers.Target) (providers.Target, bool) {
	pin := reg.Row(pinProfile())
	switch {
	case pin == nil || pin == t.Provider:
		return providers.Target{}, false
	case pin.Auth.Login == "copilot":
		model, _ := providers.CopilotModel(reg)
		return providers.Target{Provider: pin, Model: model}, true
	case pin.DatabricksUsable():
		return providers.Target{Provider: pin, Model: tierModel(pin, models.Opus)}, true
	}
	return providers.Target{}, false
}

// pinProfile is copilot's route profile, else its fallback route's (a {github, account} route names none).
func pinProfile() string {
	rec := vault.GetAgentRecord("copilot")
	return cmp.Or(rec.EffectiveRoute().Profile, rec.EffectiveFallbackRoute().Profile)
}

// typedOffPin reports a target row other than copilot's record profile: the caller typed it.
func typedOffPin(t providers.Target) bool {
	return t.Provider != nil && t.Provider.Name != pinProfile()
}

// tierModel is copilot's Databricks tier model at alias on p (router table), else p's own alias model.
func tierModel(p *providers.Provider, alias string) string {
	return cmp.Or(router.DatabricksModel("copilot", alias, p), p.Models[alias])
}

// copilotDatabricksFallback: session, else pinned, else first credentialed Databricks row; detail: why not GitHub.
func copilotDatabricksFallback(ctx context.Context, reg *providers.Registry, detail string) agent.RouteInfo {
	for _, name := range fallbackProfiles(ctx, reg) {
		if p := reg.Providers[name]; p.DatabricksUsable() {
			reason := "copilot: " + detail + " — using databricks profile " + p.Name
			return agent.RouteInfo{Agent: "copilot", Route: agent.RouteDatabricks, Provider: p.Name, Model: tierModel(p, models.Opus),
				Reason: reason, OK: true}
		}
	}
	return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone,
		Reason: "copilot: " + detail + "; no credit-free route for copilot (no Databricks profile signed in)"}
}

// fallbackProfiles is the fallback's try order: the session's (else claude's pinned) profile, then each Databricks row.
func fallbackProfiles(ctx context.Context, reg *providers.Registry) []string {
	lead := providers.SessionLeadContext(ctx, nil, true)
	names := []string{lead}
	for _, n := range providers.DatabricksProfiles(reg) {
		if n != lead {
			names = append(names, n)
		}
	}
	return names
}

// copilotGithubRow is the github profile copilot's own login runs on: the record's pinned row, else the first one.
func copilotGithubRow(reg *providers.Registry) (*providers.Provider, bool) {
	if _, pin := providers.CopilotLogin(reg); pin != "" {
		return reg.Providers[pin], true
	}
	return reg.LoginProfile("github", "copilot")
}

// describeCopilotRoute: GitHub login on a workable pin, else Databricks fallback, else anthropic-key BYOK, else none.
func describeCopilotRoute(ctx context.Context, reg *providers.Registry) agent.RouteInfo {
	if reg == nil {
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone, Reason: "no route for copilot: no provider registry"}
	}
	if pin := pinProfile(); reg.Providers[pin].DatabricksUsable() {
		p := reg.Providers[pin]
		t := agent.PinnedTarget(ctx, "copilot", providers.Target{Provider: p, Model: tierModel(p, models.Opus)})
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteDatabricks, Provider: p.Name, Model: t.Model,
			Reason: "copilot: pinned databricks profile " + p.Name, OK: true}
	}
	detail, workable := pinnedHeadroom(ctx, "")
	if a, ok := copilotauth.AuthSource(); ok && workable {
		if p, ok := copilotGithubRow(reg); ok {
			model, _ := providers.CopilotModel(reg)
			return agent.RouteInfo{Agent: "copilot", Route: agent.RouteCopilotLogin, Provider: p.Name, Model: model,
				Reason: "github-copilot (" + providers.AuthState{Account: a.Account, Source: a.Source}.Label() + ")", OK: true}
		}
	}
	if ri := copilotDatabricksFallback(ctx, reg, detail); ri.OK {
		return ri
	}
	if p, ok := reg.KeyProfile("anthropic"); ok && providers.CredentialPresent(p) {
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteByokAnthropic, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "BYOK via ANTHROPIC_API_KEY", OK: true}
	}
	s := copilotSchema()
	return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone, Reason: "no route for copilot: " + s.CLI.ConfigureHint}
}

// copilotRouteDetail is copilot's resolved dispatch, never a credential; its GitHub login adds model source and pins.
func copilotRouteDetail(ctx context.Context, reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := copilotSchema().DescribeRoute(ctx, reg)
	if !ri.OK {
		return ri.Reason
	}
	detail := fmt.Sprintf("dispatch=%s provider=%s model=%s (%s)", ri.Route, ri.Provider, ri.Model, ri.Reason)
	if ri.Route == agent.RouteCopilotLogin {
		_, source := providers.CopilotModel(reg)
		if m, v := providers.EnvModel(ctx, "copilot"); m != "" {
			source = "env " + v
		}
		rec := vault.GetAgentRecord("copilot")
		effort, context := cmp.Or(rec.Effort, "default"), cmp.Or(rec.Context, "default")
		detail += fmt.Sprintf(" source=%s effort=%s context=%s", source, effort, context)
	}
	return detail
}

// RouteStep is one link of copilot's route chain: what it runs on, when it applies, and whether it is usable now.
type RouteStep struct {
	Label, When string
	OK          bool
}

// RouteChain is copilot's routes in describeCopilotRoute's try order, offline (no credit lookup).
func RouteChain(ctx context.Context, reg *providers.Registry) []RouteStep {
	if reg == nil {
		return nil
	}
	var steps []RouteStep
	pin := pinProfile()
	if p := reg.Providers[pin]; p != nil && p.Preset == providers.PresetDatabricks {
		steps = append(steps, RouteStep{Label: "databricks-" + pin, When: "pinned", OK: p.DatabricksUsable()})
	}
	a, ok := copilotauth.AuthSource()
	gh := "GitHub"
	if a.Account != "" {
		gh += " " + a.Account
	}
	steps = append(steps, RouteStep{Label: gh, When: fmt.Sprintf("under %.0f%% of credits", copilotWorkablePct), OK: ok})
	for _, name := range fallbackProfiles(ctx, reg) {
		if p := reg.Providers[name]; name != pin && p.DatabricksUsable() {
			steps = append(steps, RouteStep{Label: "databricks-" + name, When: "fallback", OK: true})
			break
		}
	}
	if p, ok := reg.KeyProfile("anthropic"); ok {
		steps = append(steps, RouteStep{Label: proc.EnvAnthropicAPIKey, When: "BYOK", OK: providers.CredentialPresent(p)})
	}
	return steps
}

// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, GH_TOKEN); blanked BYOK vars cannot redirect it.
func copilotLogin(model, login string, h *agent.Headless) ([]string, []string, error) {
	// no pin: blank (copilot reads "" as unset) so copilot's own /model default applies
	if model == "auto" {
		model = ""
	}
	env := proc.Values{
		"COPILOT_MODEL": model, proc.EnvCopilotProviderBaseURL: "", "COPILOT_OFFLINE": "", proc.EnvCopilotProvidersConfig: "",
	}
	tok, err := copilotLaunchTokenFor(login)
	if err != nil {
		return nil, nil, err
	}
	// the harness's chosen account beats any inherited token
	if tok != "" {
		env = env.With(providers.CopilotTokenEnv, tok)
	}
	return env.Env(), copilotArgv(h), nil
}

// copilotArgv is the headless argv for h; an interactive launch (nil) gets its pins from RenderSettings.
func copilotArgv(h *agent.Headless) []string {
	if h == nil {
		return nil
	}
	argv := []string{"--prompt", h.Prompt}
	if h.Tools && h.Autonomous {
		argv = append(argv, "--allow-all-tools")
	}
	return argv
}

// RenderSettings is the default launch env/args, then --reasoning-effort/--context on an interactive launch.
func (e copilotEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string,
) ([]string, []string, []string, func(), error) {
	env, argv, emulated, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if err != nil || s.Headless {
		return env, argv, emulated, cleanup, err
	}
	effort, note := modelEffort(s)
	if note != "" {
		emulated = append(emulated, note)
	}
	if effort != "" {
		argv = append(argv, "--reasoning-effort", effort)
	}
	if c := s.Record.Context; c != "" {
		argv = append(argv, "--context", c)
	}
	return env, argv, emulated, cleanup, nil
}

// copilotWire wires BYOK; the target's context window (providers.ContextWindow) sizes the prompt cap.
func copilotWire(r agent.WireRoute, h *agent.Headless) ([]string, []string, error) {
	p, model := r.Provider, r.Model
	typ, base, wire, modelID, err := copilotEndpoint(p, r.Dialect, r.Base, model, r.Routed)
	if err != nil {
		return nil, nil, err
	}
	client := r.Target.ClientModel()
	window, output := copilotCaps(cmp.Or(r.Target.ContextTokens, providers.Window(r.Target.Provider, client)), model)
	env := proc.Values{
		proc.EnvCopilotProviderBaseURL:         base,
		proc.EnvCopilotProviderType:            typ,
		"COPILOT_MODEL":                        model,
		proc.EnvCopilotProviderMaxOutputTokens: strconv.Itoa(output),
		"COPILOT_OFFLINE":                      "true",
		// Blank every BYOK credential var so a token exported in the parent shell cannot outrank this route.
		proc.EnvCopilotProviderAPIKey: "", proc.EnvCopilotProviderBearerToken: "", proc.EnvCopilotProviderAPIKeyCommand: "",
	}
	if wire == providers.ProtocolResponses {
		env = env.With(proc.EnvCopilotProviderWireAPI, "responses")
	}
	if modelID != "" {
		env = env.With(proc.EnvCopilotProviderModelID, modelID)
	}
	// Prompt budget = window minus the output cap and a 10% margin, so copilot compacts before the endpoint refuses.
	if n := models.PromptTokens(window, output); n > 0 {
		env = env.With(proc.EnvCopilotProviderMaxPromptTokens, strconv.Itoa(n))
	}
	return env.Merge(copilotCredEnv(p, r.Cred)).Env(), append(copilotArgv(h), mcpInstructionsArgs()...), nil
}

// minMCPInstructionsCLI is the oldest copilot CLI with --allow-all-mcp-server-instructions.
const minMCPInstructionsCLI = "1.0.91"

// installedVersion is the copilot CLI's version, "" when unknown; tests replace it.
var installedVersion = func() string {
	v, _ := tools.Version("copilot")
	return v
}

// mcpInstructionsArgs lets MCP servers' instructions reach the model: offline mode empties that allowlist.
func mcpInstructionsArgs() []string {
	if c, ok := textutil.CompareVersions(installedVersion(), minMCPInstructionsCLI); ok && c >= 0 {
		return []string{"--allow-all-mcp-server-instructions"}
	}
	return nil
}

// copilotEndpoint picks the BYOK type and base; a non-Claude model on anthropic takes its openai|responses wire.
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
	// the router's openai|responses passthrough
	case routed:
		return "openai", base + "/" + w, w, runtimeModelID(canonical, model), nil
	}
	// --direct: the profile's own endpoint for that wire
	if b, ok = p.Route(w); !ok {
		return "", "", "", "", fmt.Errorf("copilot: profile %s has no %s route for %s", p.Name, w, model)
	}
	return "openai", b, w, "", nil
}

// copilotCredEnv is the BYOK credential and the provider's extra headers.
func copilotCredEnv(p *providers.Provider, cred string) proc.Values {
	env := proc.Values{}
	if cred != "" {
		// Bearer precedence is copilot's own rule; sending the key on both headers trips strict gateways.
		if p.Auth.HeaderStyle() == providers.HeaderBearer {
			env[proc.EnvCopilotProviderBearerToken] = cred
		} else {
			env[proc.EnvCopilotProviderAPIKey] = cred
		}
	}
	if len(p.Headers) > 0 {
		env[proc.EnvCopilotProviderHeaders] = providers.HeaderLines(p.Headers)
	}
	return env
}

// ValidateCopilotPins checks pins against their live catalog: unreachable warns (pin written), an unknown id errors.
func ValidateCopilotPins(ctx context.Context, providerFlag string, pins []string) (warn string, err error) {
	fallback := providerFlag
	if fallback == "" {
		fallback = providers.SessionLeadContext(ctx, nil, true)
	}
	reg, rerr := providers.Load()
	if rerr != nil {
		return fmt.Sprintf("could not verify pins: provider registry unreadable (pin written anyway): %v", rerr), nil
	}
	v := &pinValidator{reg: reg, fallback: fallback, served: map[string][]string{}}
	for _, p := range pins {
		if err := v.check(ctx, p); err != nil {
			return "", err
		}
	}
	return v.firstWarn, nil
}

// pinValidator carries ValidateCopilotPins' per-run state: the github catalog is fetched at most once.
type pinValidator struct {
	reg                  *providers.Registry
	fallback             string
	ids                  []string
	fetched, catalogDown bool
	// the first unverifiable-pin warning; later pins are still checked
	firstWarn string
	served    map[string][]string
}

// check validates one "<agent>=<spec>" pin; a non-nil error is a hard refusal.
func (v *pinValidator) check(ctx context.Context, p string) error {
	_, spec, ok := strings.Cut(p, "=")
	if !ok {
		return nil
	}
	name, model := v.fallback, strings.TrimSpace(spec) // a plain id; the profile is the route
	if err := agent.PrefixedRefusal(v.reg, "copilot", model); err != nil {
		return err
	}
	if dp := v.reg.Providers[name]; dp != nil && dp.Preset == providers.PresetDatabricks {
		w, err := validateDatabricksCopilotPin(ctx, v.reg, name, model, v.served)
		if err != nil {
			return err
		}
		v.firstWarn = cmp.Or(v.firstWarn, w)
		return nil
	}
	gh, err := v.reg.Get(name)
	if err != nil || gh.Provider != "github" {
		return nil
	}
	v.fetchCatalog(ctx)
	if v.catalogDown || slices.Contains(v.ids, model) {
		return nil
	}
	return unknownGithubModel(model, gh.Name, v.ids)
}

// fetchCatalog loads the live github-copilot catalog once, remembering a failure as a warning.
func (v *pinValidator) fetchCatalog(ctx context.Context) {
	if v.fetched {
		return
	}
	v.fetched = true
	var err error
	if v.ids, err = copilotModelIDs(ctx); err != nil {
		v.catalogDown = true
		v.firstWarn = cmp.Or(v.firstWarn, fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err))
	}
}

// unknownGithubModel is the refusal for a model missing from profile's live catalog, naming near matches.
func unknownGithubModel(model, profile string, ids []string) error {
	if near := nearMatches(model, ids); len(near) > 0 {
		return fmt.Errorf("%q is not a github-copilot model; did you mean: %s?", model, strings.Join(near, ", "))
	}
	return fmt.Errorf("%q is not a github-copilot model (run `claude-code models --live --profile %s` to see the catalog)", model, profile)
}

// validateDatabricksCopilotPin checks model against name's serving endpoints; served caches each profile's ids.
func validateDatabricksCopilotPin(
	ctx context.Context, reg *providers.Registry, name, model string, served map[string][]string,
) (string, error) {
	ids, warn, ok := servedIDs(ctx, reg, name, served)
	if !ok {
		return warn, nil
	}
	want := []string{model, models.DatabricksPrefix + model}
	if c, _, ok := models.Resolve(model); ok {
		want = append(want, c)
	}
	for _, id := range ids {
		c, _, _ := models.Resolve(id)
		if slices.Contains(want, id) || (c != "" && slices.Contains(want, c)) {
			return "", nil
		}
	}
	hint := fmt.Sprintf("run `claude-code models --live --profile %s` to see the endpoints", name)
	if near := nearMatches(model, ids); len(near) > 0 {
		return "", fmt.Errorf("%q is not a %s serving endpoint; did you mean: %s? (%s)",
			model, name, strings.Join(near, ", "), hint)
	}
	return "", fmt.Errorf("%q is not a %s serving endpoint (%s)", model, name, hint)
}

// servedIDs lists name's serving endpoint ids once per validation; a failed listing is ok=false, warned once.
func servedIDs(
	ctx context.Context, reg *providers.Registry, name string, served map[string][]string,
) (ids []string, warn string, ok bool) {
	if ids, seen := served[name]; seen {
		return ids, "", ids != nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	cat, err := CopilotModelSource(ctx, reg, name)
	cancel()
	if err != nil {
		// a nil entry marks a failed listing, so later pins don't wait on it again
		served[name] = nil
		return nil, fmt.Sprintf("could not verify against %s serving endpoints (pin written anyway): %v", name, err), false
	}
	ids = make([]string, 0, len(cat.Rows))
	for _, r := range cat.Rows {
		ids = append(ids, r.ID)
	}
	served[name] = ids
	return ids, "", true
}

// nearMatches is a cheap substring match, not full edit distance: enough to catch a typo'd suffix/prefix.
func nearMatches(want string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if strings.Contains(id, want) || strings.Contains(want, id) {
			out = append(out, id)
		}
	}
	return out
}

// copilotModelIDs lists the copilot catalog's model ids (sealed snapshot first).
func copilotModelIDs(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	list, err := Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range list {
		if m.Enabled {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func modelEffort(s agent.Settings) (string, string) {
	if s.Effort == "" {
		return "", ""
	}
	id := models.Strip1M(s.Target.Model)
	if id == "" { // the CLI's own default model: the effort is the caller's knob for it
		return s.Effort, ""
	}
	if id == agent.AutoModel {
		return "", "copilot: auto takes no reasoning effort; not sent"
	}
	accepted := catalogEfforts(id)
	// a gateway-served (BYOK) row; a nil, github or own-login row reads the Copilot catalog
	if p := s.Target.Provider; p != nil && !catalog.IsGithubProfile(p) && p.Auth.Login == "" {
		accepted = servedEfforts(id)
	}
	native := nearestEffort(accepted, s.Effort)
	switch {
	case native == "":
		return "", "copilot: " + s.Target.Model + " takes no reasoning effort; not sent"
	case native != s.Effort:
		return native, "effort " + s.Effort + "→" + native
	}
	return native, ""
}

// runtimeModelID is known unless wire takes no effort: the runtime sends its default effort to any model it knows.
func runtimeModelID(known, wire string) string {
	if servedEfforts(wire) == nil {
		return wire
	}
	return known
}

// trioEfforts is what a reasoning model accepts when its source lists no levels.
var trioEfforts = []string{"low", "medium", "high"}

// catalogEfforts is id's accepted efforts on the harness scale from the Copilot catalog snapshot; nil: none or unknown.
func catalogEfforts(id string) []string {
	dotted := models.WireOrOriginal("dotted", id)
	for _, m := range effortModels() {
		if m.ID != id && m.ID != dotted {
			continue
		}
		if m.Efforts == nil && m.ReasoningEffort {
			return trioEfforts
		}
		var out []string
		for _, level := range agentid.EffortNames() {
			if slices.Contains(m.Efforts, level) {
				out = append(out, level)
			}
		}
		return out
	}
	return nil
}

// servedEfforts is what a gateway-served id accepts: Databricks Gemini and open models reject reasoning_effort.
func servedEfforts(id string) []string {
	if listed := models.AcceptedEfforts("copilot", id); listed != nil {
		return listed
	}
	switch {
	case models.FamilyKind(id) == models.FamilyClaude:
		return agentid.NativeEfforts(agentid.Claude)
	case models.IsOpenAIReasoning(id):
		return []string{"low", "medium", "high", "xhigh"}
	case models.FamilyKind(id) == models.FamilyGPT:
		return trioEfforts
	}
	return nil
}

// nearestEffort is effort when accepted lists it, else the closest accepted level (weaker on a tie); "" when none.
func nearestEffort(accepted []string, effort string) string {
	if slices.Contains(accepted, effort) {
		return effort
	}
	scale := agentid.EffortNames()
	want := slices.Index(scale, effort)
	if want < 0 {
		return ""
	}
	best, bestDist := "", len(scale)
	for _, level := range accepted {
		i := slices.Index(scale, level)
		if d := max(want-i, i-want); i >= 0 && (d < bestDist || d == bestDist && i < want) {
			best, bestDist = level, d
		}
	}
	return best
}

var effortModels = cachedEffortModels

// Model capabilities are account-independent; this read must not discover or resolve launch credentials.
func cachedEffortModels() []SDKModel {
	raw, found, err := vault.ReadSecret(copilotauth.CatalogRecord)
	if err != nil || !found {
		return nil
	}
	var snap copilotauth.Snapshot[SDKModel]
	if json.Unmarshal([]byte(raw), &snap) != nil || snap.Schema != 1 {
		return nil
	}
	return snap.Models
}
