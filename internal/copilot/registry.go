package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Seams for tests: model discovery, the running router and the router's session tokens.
var (
	discoverModels = discoverCopilotEndpoints
	mintSession    = gateway.MintSessionToken
	revokeSession  = gateway.RevokeSessionToken
	runningRouter  = gateway.Running
)

// StubRegistry makes registry launches discover ids and mint tok offline, for tests in other packages.
func StubRegistry(tok string, ids ...string) (restore func()) {
	oldDiscover, oldMint, oldRevoke := discoverModels, mintSession, revokeSession
	eps := make([]CopilotEndpoint, len(ids))
	for i, id := range ids {
		eps[i] = CopilotEndpoint{ID: id}
	}
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		return eps, nil
	}
	mintSession = func(context.Context, string, int) (string, error) { return tok, nil }
	revokeSession = func(context.Context, string) error { return nil }
	return func() { discoverModels, mintSession, revokeSession = oldDiscover, oldMint, oldRevoke }
}

// copilotRegistryRoute is the multi-provider SDK wiring for one run: in-memory providers, a session token, no file.
type copilotRegistryRoute struct {
	providers []copilotRegistryProvider
	models    []copilotRegistryModel
	ref       string // the selected provider/id
	token     string
	home      copilotPrivateHome
}

// prepareCopilotRegistry discovers what t's profile serves, selects t.Model and mints a token for this process.
func prepareCopilotRegistry(ctx context.Context, t providers.Target) (*copilotRegistryRoute, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	eps, err := discoverModels(ctx, reg, t.Provider)
	if err != nil {
		return nil, fmt.Errorf("copilot: discover %s models: %w", t.Provider.Name, err)
	}
	base, err := agent.EnsureRouter(ctx)
	if err != nil {
		return nil, err
	}
	var requested []string // empty stays nil so the pin or default applies
	if m := models.Strip1M(t.Model); m != "" {
		requested = []string{m}
	}
	cr, ref, err := copilotSelect(ctx, reg, t.Provider, eps, base, copilotChoice(requested, ""))
	if cr.Note != "" {
		wire.EventLogFor(ctx).Emit("note", cr.Note)
	}
	if err != nil {
		return nil, fmt.Errorf("copilot: %w", err)
	}
	home, err := prepareCopilotHome(false)
	if err != nil {
		return nil, err
	}
	for _, w := range home.Warnings {
		wire.EventLogFor(ctx).Emit("note", w)
	}
	tok, err := mintSession(ctx, t.Provider.Name, os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("copilot: mint session token: %w", err)
	}
	return &copilotRegistryRoute{providers: cr.Providers, models: cr.Models, ref: ref, token: tok, home: home}, nil
}

// apply puts the route on opts; the token reaches the SDK only as a callback result.
func (rt *copilotRegistryRoute) apply(o *sdkOptions) {
	o.Providers, o.Models, o.Model = rt.providers, rt.models, rt.ref
	o.Token = func() (string, error) { return rt.token, nil }
	o.GitHubToken, o.UseLoggedInUser, o.Env = "", false, nil
	o.ChildEnv, o.ChildUnset, o.ChildUnsetPrefix = rt.home.Env, rt.home.Unset, proc.CopilotProviderPrefix
}

// close revokes the run's token; the daemon also expires it, so a failure is not worth failing the run.
func (rt *copilotRegistryRoute) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = revokeSession(ctx, rt.token)
}

// copilotRegistryProvider is one named upstream: Type anthropic|openai, WireAPI "" (anthropic), completions or responses.
type copilotRegistryProvider struct {
	Name    string
	Type    string
	WireAPI string
	BaseURL string
	Bearer  bool // credential goes in bearerToken, else apiKey
	Headers map[string]string
}

// copilotRegistryModel is one selectable entry; the CLI selects it as Provider + "/" + ID.
type copilotRegistryModel struct {
	ID                     string // canonical
	Provider               string
	WireModel              string // the Databricks endpoint id
	ModelID                string // copilot's catalog form, so its per-model settings apply
	Name                   string
	MaxPromptTokens        int
	MaxContextWindowTokens int
	MaxOutputTokens        int
}

// copilotRegistry is the credential-free registry data; the SDK path maps it in memory, the CLI path renders it.
type copilotRegistry struct {
	Profile   string
	Providers []copilotRegistryProvider
	Models    []copilotRegistryModel
	Note      string // one line naming the endpoints dropped because the profile has no route for them; "" when none
}

// Ref is the CLI's selection string for m.
func (m copilotRegistryModel) Ref() string { return m.Provider + "/" + m.ID }

// copilotWireRoute is the registry shape of one wire; its router path is copilot's CLI.RouterSuffix.
var copilotWireRoute = map[string]struct{ typ, wireAPI string }{
	providers.ProtocolAnthropic: {"anthropic", ""},
	providers.ProtocolOpenAI:    {"openai", "completions"},
	providers.ProtocolResponses: {"openai", "responses"},
}

// copilotBuildRegistry maps profile p's endpoints to registry data against routerBase (the router root, no /p/<profile>).
// An endpoint whose canonical id repeats an earlier one is dropped, so every id selects one model.
func copilotBuildRegistry(profile string, p *providers.Provider, eps []CopilotEndpoint, routerBase string) copilotRegistry {
	r := copilotRegistry{Profile: profile}
	base := strings.TrimRight(routerBase, "/") + "/p/" + profile
	seen, used := map[string]bool{}, map[string]bool{}
	var dropped []string
	for _, ep := range eps {
		wire, canonical, ok, _ := copilotEndpointWire(p, ep.ID)
		if !ok {
			dropped = append(dropped, ep.ID)
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		name := profile + "-" + wire
		if !used[wire] {
			used[wire] = true
			r.Providers = append(r.Providers, newCopilotRegistryProvider(name, wire, base, p))
		}
		window, output := copilotCaps(providers.ContextWindow(p, ep.ID), ep.ID)
		r.Models = append(r.Models, copilotRegistryModel{
			ID: canonical, Provider: name, WireModel: ep.ID, ModelID: wellKnownModel(canonical), Name: ep.Name,
			MaxPromptTokens: models.PromptTokens(window, output), MaxContextWindowTokens: window, MaxOutputTokens: output,
		})
	}
	if len(dropped) > 0 {
		r.Note = fmt.Sprintf("copilot: %s has no route for %d endpoint(s), left out of the model menu: %s", profile, len(dropped),
			strings.Join(dropped, ", "))
	}
	return r
}

func newCopilotRegistryProvider(name, wire, base string, p *providers.Provider) copilotRegistryProvider {
	rt := copilotWireRoute[wire]
	spec := copilotSchema()
	return copilotRegistryProvider{Name: name, Type: rt.typ, WireAPI: rt.wireAPI, BaseURL: base + spec.CLI.RouterSuffix[wire],
		Bearer: wire != providers.ProtocolAnthropic, Headers: p.Headers}
}

func canonicalOf(endpoint string) string {
	if c, _, ok := models.Resolve(endpoint); ok {
		return c
	}
	return strings.TrimPrefix(endpoint, models.DatabricksPrefix)
}

type jsonProvider struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	WireAPI     string            `json:"wireApi,omitempty"`
	BaseURL     string            `json:"baseUrl"`
	APIKey      string            `json:"apiKey,omitempty"`
	BearerToken string            `json:"bearerToken,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type jsonModel struct {
	ID                     string `json:"id"`
	Provider               string `json:"provider"`
	WireModel              string `json:"wireModel"`
	ModelID                string `json:"modelId"`
	Name                   string `json:"name"`
	MaxPromptTokens        int    `json:"maxPromptTokens,omitempty"`
	MaxContextWindowTokens int    `json:"maxContextWindowTokens,omitempty"`
	MaxOutputTokens        int    `json:"maxOutputTokens,omitempty"`
}

// renderCopilotRegistry is the CLI's COPILOT_PROVIDERS_CONFIG JSON with cred (the session token) on every provider.
func renderCopilotRegistry(r copilotRegistry, cred string) ([]byte, error) {
	if len(r.Models) == 0 {
		return nil, errors.New("copilot registry: no models to render")
	}
	out := struct {
		Providers []jsonProvider `json:"providers"`
		Models    []jsonModel    `json:"models"`
	}{Providers: []jsonProvider{}, Models: []jsonModel{}}
	for _, p := range r.Providers {
		jp := jsonProvider{Name: p.Name, Type: p.Type, WireAPI: p.WireAPI, BaseURL: p.BaseURL, Headers: p.Headers}
		if p.Bearer {
			jp.BearerToken = cred
		} else {
			jp.APIKey = cred
		}
		out.Providers = append(out.Providers, jp)
	}
	for _, m := range r.Models {
		out.Models = append(out.Models, jsonModel(m))
	}
	return json.MarshalIndent(out, "", "  ")
}

// Named selection failures; callers match with errors.Is.
var (
	errUnknownModel      = errors.New("model is not in the registry")
	errAmbiguousModel    = errors.New("model selection is ambiguous")
	errSavedModelGone    = errors.New("saved model left the registry")
	errNoModels          = errors.New("registry has no models")
	errRepeatedSelection = errors.New("model selected more than once")
)

// modelChoice is every place a model can come from, highest priority first: Requested (each --model given),
// Pin (the vault pin value); none set picks the default.
type modelChoice struct {
	Requested []string
	Pin       string
}

// Select maps a harness alias | canonical id | wire id | provider/id to the registry's provider/id.
func (r copilotRegistry) Select(c modelChoice) (string, error) {
	if len(r.Models) == 0 {
		return "", errNoModels
	}
	switch {
	case len(c.Requested) > 1:
		return "", fmt.Errorf("%w: %s", errRepeatedSelection, strings.Join(c.Requested, ", "))
	case len(c.Requested) == 1:
		return r.find(c.Requested[0], errUnknownModel)
	case c.Pin != "":
		return r.find(c.Pin, errSavedModelGone)
	}
	return r.defaultRef(), nil
}

// defaultRef is the sonnet alias's model when the profile serves it, else the first model.
func (r copilotRegistry) defaultRef() string {
	if canonical, ok := models.Alias(models.Sonnet); ok {
		for _, m := range r.Models {
			if m.ID == canonical {
				return m.Ref()
			}
		}
	}
	return r.Models[0].Ref()
}

// find matches s by provider/id, then id, then wire id, then its canonical resolution; a miss wraps miss.
func (r copilotRegistry) find(s string, miss error) (string, error) {
	s = strings.TrimSpace(s)
	canonical, _, _ := models.Resolve(s)
	for _, match := range []func(copilotRegistryModel) bool{
		func(m copilotRegistryModel) bool { return m.Ref() == s },
		func(m copilotRegistryModel) bool { return m.ID == s },
		func(m copilotRegistryModel) bool { return m.WireModel == s },
		func(m copilotRegistryModel) bool { return canonical != "" && m.ID == canonical },
	} {
		var hits []string
		for _, m := range r.Models {
			if match(m) {
				hits = append(hits, m.Ref())
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			return "", fmt.Errorf("%w: %q matches %s", errAmbiguousModel, s, strings.Join(hits, ", "))
		}
	}
	return "", fmt.Errorf("%w: %q (profile %s)", miss, s, r.Profile)
}

// copilotEndpointWire is the passthrough wire for model on the Databricks row p (providers.WireFor) and its canonical id; a
// Claude model, or any model on another row, rides anthropic. ok is false, with why, when p has no route serving it.
func copilotEndpointWire(p *providers.Provider, model string) (wire, canonical string, ok bool, why string) {
	canonical = canonicalOf(model)
	if p.Preset != providers.PresetDatabricks || models.FamilyKind(canonical) == models.FamilyClaude {
		return providers.ProtocolAnthropic, canonical, true, ""
	}
	wire, ok, why = providers.WireFor(p, model)
	return wire, canonical, ok, why
}

// copilotMaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const copilotMaxOutputTokens = 32_000

// copilotCaps returns id's context window and output cap: window and copilotMaxOutputTokens, lowered by any documented catalog limit.
func copilotCaps(window int, id string) (ctx, output int) {
	ctx, output = window, copilotMaxOutputTokens
	l, ok := models.Limits(id)
	if !ok {
		return ctx, output
	}
	if l.Context > 0 { // catalog limits only lower the gated window and the output cap, never raise them
		ctx = min(ctx, l.Context)
	}
	if l.Output > 0 {
		output = min(output, l.Output)
	}
	return ctx, output
}

// CopilotRegistryFile is the 0600 registry file of the process pid inside the runtime dir.
func CopilotRegistryFile(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+".json")
}

// CopilotRuntimeDir creates (0700) and verifies the registry directory: a real dir, owned by this uid, mode 0700,
// and memory-backed where the platform allows (see backingCheck).
func CopilotRuntimeDir() (string, error) {
	dir, err := paths.CopilotRuntimeDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("copilot registry: runtime dir: %w", err)
	}
	if err := checkDir(dir); err != nil {
		return "", err
	}
	if err := backingCheck(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// checkDir fails unless dir is a non-symlink directory owned by the current uid with mode exactly 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("copilot registry: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("copilot registry: %s has mode %o, want 0700", dir, perm)
	}
	if uid, ok := paths.OwnerUID(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("copilot registry: %s is not owned by uid %d", dir, os.Getuid())
	}
	return nil
}

// copilotChoice is the model choice: the requested models, else the agent pin, else the profile default model.
func copilotChoice(requested []string, profileModel string) modelChoice {
	pin := agent.AgentModelPin("copilot")
	if m := models.Strip1M(profileModel); len(requested) == 0 && pin == "" && m != "" {
		requested = []string{m}
	}
	return modelChoice{Requested: requested, Pin: pin}
}

// copilotRegistryBanner is the one-line launch notice; GitHub models are unreachable by construction in this mode.
const copilotRegistryBanner = "copilot: GitHub models are unavailable in this mode"

// copilotRegistryEligible: a keyed Databricks profile the router can mint a session for (not direct, not a login,
// not auth none/cli — the router cannot authenticate those rows), except armed on macOS, which runs routed BYOK.
func copilotRegistryEligible(direct bool, t providers.Target) bool {
	p := t.Provider
	return p != nil && !direct && p.Preset == providers.PresetDatabricks && p.Auth.Login == "" &&
		p.Auth.Method() != providers.AuthNone && p.Auth.Method() != providers.AuthCLI && !armedOnMacOS()
}

// armedOnMacOS is an overlay databricks:<p> run on macOS, where the registry is refused (home.go).
func armedOnMacOS() bool { return copilotGOOS == "darwin" && router.ArmProfile() != "" }

// usesCopilotRegistry reports whether t is a Databricks row whose copilot launch runs on the registry.
func usesCopilotRegistry(ctx context.Context, t providers.Target) bool {
	return copilotRegistryEligible(agent.IsDirect(ctx), t)
}

// configureCopilotRegistry builds the registry launch for t; errors are fatal (no plain fallback); dry mints nothing.
func configureCopilotRegistry(
	ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool,
) (agent.RegistryLaunch, error) {
	if err := checkCopilotRegistryOS(); err != nil { // before discovery or minting
		return agent.RegistryLaunch{}, err
	}
	p := t.Provider
	requested, rest := splitModelFlags(args)
	eps, err := discoverModels(ctx, reg, p)
	if err != nil {
		return agent.RegistryLaunch{}, fmt.Errorf("copilot: list models for profile %s: %w", p.Name, err)
	}
	rb, err := copilotRouterBase(ctx, dry)
	if err != nil {
		return agent.RegistryLaunch{}, fmt.Errorf("copilot: router: %w", err)
	}
	r, ref, err := copilotSelect(ctx, reg, p, eps, rb, copilotChoice(requested, t.Model))
	if err != nil {
		return agent.RegistryLaunch{}, fmt.Errorf("copilot: %w", err)
	}
	file, files, token, err := registryFile(ctx, r, p.Name, os.Getpid(), dry)
	revoke := func() { // a launch that never minted a token is a no-op
		if token != "" {
			_ = revokeSession(context.Background(), token)
		}
	}
	if err != nil {
		revoke()
		return agent.RegistryLaunch{}, err
	}
	ph, err := prepareCopilotHome(dry)
	if err != nil {
		revoke()
		return agent.RegistryLaunch{}, err
	}
	warnings := ph.Warnings
	if r.Note != "" {
		warnings = append(warnings, r.Note)
	}
	// OFFLINE removes /login but needs the registry file at startup; the SDK path has none, so it stays here.
	return agent.RegistryLaunch{
		Env:   append([]string{"COPILOT_PROVIDERS_CONFIG=" + file, "COPILOT_OFFLINE=true"}, ph.Env...),
		Unset: ph.Unset, Argv: []string{"--model", ref}, Args: rest,
		Warnings: warnings, Files: files, Revoke: revoke, Banner: copilotRegistryBanner,
	}, nil
}

// copilotRouterBase is the router URL; dry (--print-env) only reads a running router and never starts one.
func copilotRouterBase(ctx context.Context, dry bool) (string, error) {
	if !dry {
		return agent.EnsureRouter(ctx)
	}
	if st, ok := runningRouter(ctx); ok {
		return gateway.ViaBase(gateway.Base(st.Port)), nil
	}
	return gateway.ViaBase(gateway.Base(gateway.DefaultPort)), nil
}

// prepareRegistry mints the token and renders the registry as a launch file in the verified runtime dir; Exec writes it.
func prepareRegistry(ctx context.Context, r copilotRegistry, profile string, pid int,
) (agent.LaunchFile, string, error) {
	token, err := mintSession(ctx, profile, pid)
	if err != nil {
		return agent.LaunchFile{}, "", fmt.Errorf("copilot: session token for profile %s: %w", profile, err)
	}
	data, err := renderCopilotRegistry(r, token)
	if err != nil {
		return agent.LaunchFile{}, token, fmt.Errorf("copilot: render registry: %w", err)
	}
	dir, err := CopilotRuntimeDir()
	if err != nil {
		return agent.LaunchFile{}, token, fmt.Errorf("copilot: %w", err)
	}
	return agent.LaunchFile{Path: CopilotRegistryFile(dir, pid), Data: data}, token, nil
}

// registryFile is the registry file path and its launch file; dry only computes the path (--print-env creates nothing).
func registryFile(
	ctx context.Context, r copilotRegistry, profile string, pid int, dry bool,
) (file string, files []agent.LaunchFile, token string, err error) {
	if !dry {
		f, tok, err := prepareRegistry(ctx, r, profile, pid)
		if err != nil {
			return "", nil, tok, err
		}
		return f.Path, []agent.LaunchFile{f}, tok, nil
	}
	glob := paths.CopilotRegistryGlob()
	if glob == "" {
		return "", nil, "", errors.New("copilot: no registry runtime directory")
	}
	return CopilotRegistryFile(filepath.Dir(glob), pid), nil, "", nil
}

// splitModelFlags removes every --model <v> / --model=<v> from args and returns the values in order.
func splitModelFlags(args []string) (models, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--model" && i+1 < len(args):
			models = append(models, args[i+1])
			i++
		case strings.HasPrefix(a, "--model="):
			models = append(models, strings.TrimPrefix(a, "--model="))
		default:
			rest = append(rest, a)
		}
	}
	return models, rest
}

// copilotSelect builds the registry for p behind the router and selects c; a model miss on a snapshot list relists once.
func copilotSelect(ctx context.Context, reg *providers.Registry, p *providers.Provider, eps []CopilotEndpoint,
	base string, c modelChoice,
) (copilotRegistry, string, error) {
	routed := *p
	routed.Headers = map[string]string{gateway.AgentHeader: "copilot"} // the router adds the provider's own headers upstream
	if id := providers.IdentityFrom(ctx); id != "" {
		routed.Headers[gateway.IdentityHeader] = id
	}
	r := copilotBuildRegistry(p.Name, &routed, eps, base)
	ref, err := r.Select(c)
	fromSnapshot := slices.ContainsFunc(eps, func(ep CopilotEndpoint) bool { return ep.Snapshot })
	if (!errors.Is(err, errUnknownModel) && !errors.Is(err, errSavedModelGone)) || !fromSnapshot {
		return r, ref, err
	}
	fresh, listErr := relistCopilotEndpoints(ctx, reg, p)
	if listErr != nil {
		fmt.Fprintf(os.Stderr, "copilot: %s relist failed: %s\n", p.Name, serving.TruncateMsg([]byte(listErr.Error())))
		return r, "", err
	}
	r = copilotBuildRegistry(p.Name, &routed, fresh, base)
	ref, err = r.Select(c)
	return r, ref, err
}
