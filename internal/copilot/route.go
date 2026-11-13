package copilot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// copilotGate keeps t unless copilot's GitHub login is unworkable; then the Databricks fallback, never another account.
func copilotGate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	if t.Provider == nil || t.Provider.Auth.Login != "copilot" {
		return t, "", nil
	}
	// the target GitHub row's own login: an explicit --profile personal is gated on that account, not the copilot pin
	detail, ok := pinnedHeadroom(context.Background(), t.Provider.Login)
	if ok {
		return t, "", nil
	}
	if arm := router.ArmProfile(); arm != "" { // the route's chain picks the next hop; a silent Databricks move would leak
		return providers.Target{}, "", fmt.Errorf("copilot: %s; GitHub hop refused inside databricks:%s", detail, arm)
	}
	ri := copilotDatabricksFallback(reg, detail)
	ft, ok := agent.RouteTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	if strings.HasPrefix(t.Model, models.DatabricksPrefix) { // a Databricks id moves only to a profile serving it
		if _, why := agent.ProfileServes("copilot", ft.Provider, t.Model); why != "" {
			return providers.Target{}, "", fmt.Errorf("copilot: %s; databricks profile %s %s", detail, ft.Provider.Name, why)
		}
	}
	return ft, ri.Reason, nil
}

// tierModel is copilot's Databricks tier model at alias on p (router table), else p's own alias model.
func tierModel(p *providers.Provider, alias string) string {
	return cmp.Or(router.DatabricksModel("copilot", alias, p), p.Models[alias])
}

// copilotDatabricksFallback: session, else pinned, else first credentialed Databricks row; detail: why not GitHub.
func copilotDatabricksFallback(reg *providers.Registry, detail string) agent.RouteInfo {
	for _, name := range fallbackProfiles(reg) {
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
func fallbackProfiles(reg *providers.Registry) []string {
	lead := providers.SessionLead(nil, true)
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
func describeCopilotRoute(reg *providers.Registry) agent.RouteInfo {
	if reg == nil {
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteNone, Reason: "no route for copilot: no provider registry"}
	}
	if pin := vault.GetAgentRecord("copilot").Profile; reg.Providers[pin].DatabricksUsable() { // a pinned Databricks profile wins
		p := reg.Providers[pin]
		t, _ := agent.ResolveAgentModel(agent.ModelAsk{Agent: "copilot", Target: providers.Target{Provider: p, Model: tierModel(p, models.Opus)}})
		return agent.RouteInfo{Agent: "copilot", Route: agent.RouteDatabricks, Provider: p.Name, Model: t.Model,
			Reason: "copilot: pinned databricks profile " + p.Name, OK: true}
	}
	detail, workable := pinnedHeadroom(context.Background(), "")
	if a, ok := copilotAuthSource(); ok && workable {
		if p, ok := copilotGithubRow(reg); ok {
			model, _ := providers.CopilotModel(reg)
			return agent.RouteInfo{Agent: "copilot", Route: agent.RouteCopilotLogin, Provider: p.Name, Model: model,
				Reason: "github-copilot (" + providers.AuthState{Account: a.Account, Source: a.Source}.Label() + ")", OK: true}
		}
	}
	if ri := copilotDatabricksFallback(reg, detail); ri.OK {
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
func copilotRouteDetail(reg *providers.Registry, selErr error) string {
	if selErr != nil {
		return fmt.Sprintf("(%v)", selErr)
	}
	ri := copilotSchema().DescribeRoute(reg)
	if !ri.OK {
		return ri.Reason
	}
	detail := fmt.Sprintf("dispatch=%s provider=%s model=%s (%s)", ri.Route, ri.Provider, ri.Model, ri.Reason)
	if ri.Route == agent.RouteCopilotLogin {
		_, source := providers.CopilotModel(reg)
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
func RouteChain(reg *providers.Registry) []RouteStep {
	if reg == nil {
		return nil
	}
	var steps []RouteStep
	pin := vault.GetAgentRecord("copilot").Profile
	if p := reg.Providers[pin]; p != nil && p.Preset == providers.PresetDatabricks {
		steps = append(steps, RouteStep{Label: "databricks-" + pin, When: "pinned", OK: p.DatabricksUsable()})
	}
	a, ok := copilotAuthSource()
	gh := "GitHub"
	if a.Account != "" {
		gh += " " + a.Account
	}
	steps = append(steps, RouteStep{Label: gh, When: fmt.Sprintf("under %.0f%% of credits", copilotWorkablePct), OK: ok})
	for _, name := range fallbackProfiles(reg) {
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
	if model == "auto" { // no pin: blank (copilot reads "" as unset) so copilot's own /model default applies
		model = ""
	}
	env := []string{"COPILOT_MODEL=" + model, proc.EnvCopilotProviderBaseURL + "=", "COPILOT_OFFLINE=", proc.EnvCopilotProvidersConfig + "="}
	tok, err := copilotLaunchTokenFor(login)
	if err != nil {
		return nil, nil, err
	}
	if tok != "" { // the harness's chosen account beats any inherited token
		env = append(env, providers.CopilotTokenEnv+"="+tok)
	}
	return env, copilotArgv(h), nil
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
	if s.Effort != "" {
		argv = append(argv, "--reasoning-effort", s.Effort)
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
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	window, output := copilotCaps(providers.ContextWindow(r.Target.Provider, client), model)
	env := []string{
		proc.EnvCopilotProviderBaseURL + "=" + base,
		proc.EnvCopilotProviderType + "=" + typ,
		"COPILOT_MODEL=" + model,
		proc.EnvCopilotProviderMaxOutputTokens + "=" + strconv.Itoa(output),
		"COPILOT_OFFLINE=true",
		// Blank every BYOK credential var so a token exported in the parent shell cannot outrank this route.
		proc.EnvCopilotProviderAPIKey + "=", proc.EnvCopilotProviderBearerToken + "=", proc.EnvCopilotProviderAPIKeyCommand + "=",
	}
	if wire == providers.ProtocolResponses {
		env = append(env, proc.EnvCopilotProviderWireAPI+"=responses")
	}
	if modelID != "" {
		env = append(env, proc.EnvCopilotProviderModelID+"="+modelID)
	}
	// Prompt budget = window minus the output cap and a 10% margin, so copilot compacts before the endpoint refuses.
	if n := models.PromptTokens(window, output); n > 0 {
		env = append(env, proc.EnvCopilotProviderMaxPromptTokens+"="+strconv.Itoa(n))
	}
	return append(env, copilotCredEnv(p, r.Cred)...), copilotArgv(h), nil
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
			env = append(env, proc.EnvCopilotProviderBearerToken+"="+cred)
		} else {
			env = append(env, proc.EnvCopilotProviderAPIKey+"="+cred)
		}
	}
	if len(p.Headers) > 0 {
		env = append(env, proc.EnvCopilotProviderHeaders+"="+providers.HeaderLines(p.Headers))
	}
	return env
}

// ValidateCopilotPins checks pins against their live catalog: unreachable warns (pin written), an unknown id errors.
func ValidateCopilotPins(providerFlag string, pins []string) (warn string, err error) {
	fallback := providerFlag
	if fallback == "" {
		fallback = providers.SessionLead(nil, true)
	}
	reg, rerr := providers.Load()
	if rerr != nil {
		return fmt.Sprintf("could not verify pins: provider registry unreadable (pin written anyway): %v", rerr), nil
	}
	v := &pinValidator{reg: reg, fallback: fallback, served: map[string][]string{}}
	for _, p := range pins {
		if err := v.check(p); err != nil {
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
	firstWarn            string // the first unverifiable-pin warning; later pins are still checked
	served               map[string][]string
}

// check validates one "<agent>=<spec>" pin; a non-nil error is a hard refusal.
func (v *pinValidator) check(p string) error {
	_, spec, ok := strings.Cut(p, "=")
	if !ok {
		return nil
	}
	name, model := pinTarget(spec, v.fallback)
	if dp := v.reg.Providers[name]; dp != nil && dp.Preset == providers.PresetDatabricks {
		w, err := validateDatabricksCopilotPin(v.reg, name, model, v.served)
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
	v.fetchCatalog()
	if v.catalogDown || slices.Contains(v.ids, model) {
		return nil
	}
	return unknownGithubModel(model, gh.Name, v.ids)
}

// fetchCatalog loads the live github-copilot catalog once, remembering a failure as a warning.
func (v *pinValidator) fetchCatalog() {
	if v.fetched {
		return
	}
	v.fetched = true
	var err error
	if v.ids, err = copilotModelIDs(); err != nil {
		v.catalogDown = true
		v.firstWarn = cmp.Or(v.firstWarn, fmt.Sprintf("could not verify against the live github-copilot catalog (pin written anyway): %v", err))
	}
}

// pinTarget splits a pin spec into provider and model; a bare model takes fallback as its provider.
func pinTarget(spec, fallback string) (name, model string) {
	name, model, hasProvider := strings.Cut(spec, ":")
	if !hasProvider || model == "" {
		return fallback, spec
	}
	return name, model
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
	reg *providers.Registry, name, model string, served map[string][]string,
) (string, error) {
	ids, warn, ok := servedIDs(reg, name, served)
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
func servedIDs(reg *providers.Registry, name string, served map[string][]string) (ids []string, warn string, ok bool) {
	if ids, seen := served[name]; seen {
		return ids, "", ids != nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cat, err := CopilotModelSource(ctx, reg, name)
	cancel()
	if err != nil {
		served[name] = nil // a nil entry marks a failed listing, so later pins don't wait on it again
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
func copilotModelIDs() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
