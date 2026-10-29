// Package vendors wires third-party agent CLIs (codex, gemini, opencode, copilot) to a provider target,
// shared by the interactive `claude-code run` launch and the headless agent runners.
package vendors

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vendorconfig"
)

// Spec is an agent's row (providers.AgentSpec): CLI binary, wires, router paths and pin keys.
type Spec = providers.AgentSpec

// AgentSpec is Spec under the standard name.
type AgentSpec = providers.AgentSpec

// Names lists the third-party agents in roster order.
func Names() []string {
	var out []string
	for _, s := range providers.AgentSpecs() {
		if s.Binary != "" {
			out = append(out, s.Name)
		}
	}
	return out
}

// Lookup returns the row of a third-party agent by name (claude has none).
func Lookup(name string) (Spec, bool) {
	s, ok := providers.LookupAgentSpec(name)
	return s, ok && s.Binary != ""
}

// Headless is the non-interactive form of a run; zero Tools means tools are disabled where the CLI allows it.
type Headless struct {
	Prompt     string
	Effort     string
	SessionID  string // resume this session
	Tools      bool
	Autonomous bool     // opencode: --auto
	Attach     []string // opencode: one -f per path
	WorkDir    string   // opencode: --dir; cmd.Dir alone doesn't move opencode's own cwd resolution
	NoPersist  bool     // codex: --ephemeral
	SafeMode   bool     // codex: --ignore-user-config --ignore-rules; opencode: --pure
}

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	CodexKeyEnv    = "HARNESS_CODEX_API_KEY"
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

// ResolveTarget picks the target a vendor launch uses: sel's target when it can serve the vendor
// (a login provider naming it, or a route on one of its dialects), else — only when the caller named
// no provider itself (explicit is false) — the vendor's own subscription login or first keyed
// provider with a resolvable credential. note is a one-line explanation to print on fallback.
func ResolveTarget(reg *providers.Registry, name string, sel providers.Selection, explicit bool) (target providers.Target, note string, err error) {
	t, ok := sel.Slots[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// "OpenCode defaults to the Copilot plan": opencode's own github-copilot login (unrelated to the
	// session provider) is preferred whenever it is present, ahead of a session provider that could
	// otherwise already serve opencode; an explicit --provider still wins.
	if name == "opencode" && !explicit {
		if OpencodeOwnLoginPresent() { // opencode's own Go/Zen credential: launch plain on its own config
			return providers.Target{}, "", ErrOwnLogin
		}
		if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
			return gt, gnote, nil
		}
	}
	if explicit || CanServe(name, t.Provider) {
		if name == "copilot" {
			return CopilotGate(reg, t)
		}
		return t, "", nil
	}
	if ft, fallbackNote, ok := fallbackTarget(reg, name); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
		if name == "copilot" && ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks {
			note = fallbackNote // the gate's own notice already names the account and the profile
		}
		if ft.Provider == nil {
			return ft, note, ErrOwnLogin // the vendor's own stored login: launch it plain, nothing to inject
		}
		return ft, note, nil
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if name == "opencode" || name == "copilot" {
		if ri := DescribeRoute(reg, name); !ri.OK {
			return providers.Target{}, "", errors.New(ri.Reason)
		}
	}
	return t, "", nil
}

// CopilotGate keeps t unless it is copilot's GitHub login on an unworkable pin; then the Databricks fallback and its
// notice — never another GitHub account.
func CopilotGate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	if t.Provider == nil || t.Provider.Auth.Login != "copilot" {
		return t, "", nil
	}
	detail, ok := copilotHeadroom(context.Background(), CopilotPinLogin())
	if ok {
		return t, "", nil
	}
	return CopilotFallback(reg, detail)
}

// CopilotFallback is the copilot Databricks fallback target and its notice; detail names why GitHub was refused.
func CopilotFallback(reg *providers.Registry, detail string) (providers.Target, string, error) {
	ri := copilotDatabricksFallback(reg, detail)
	ft, ok := routeTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	return ft, ri.Reason, nil
}

// orderedDatabricks lists Databricks profiles in registry order.
func orderedDatabricks(reg *providers.Registry) []string {
	return providers.DatabricksProfiles(reg)
}

// copilotDatabricksFallback: this session's (else the vault-pinned) profile when it is a signed-in Databricks row,
// else the first one in orderedDatabricks whose credential resolves, else LegNone. detail names why GitHub was refused.
func copilotDatabricksFallback(reg *providers.Registry, detail string) RouteInfo {
	pick := func(p *providers.Provider) RouteInfo {
		return RouteInfo{Agent: "copilot", Leg: LegDatabricks, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "copilot: " + detail + " — using databricks profile " + p.Name, OK: true}
	}
	pin := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	for _, name := range append([]string{pin}, orderedDatabricks(reg)...) {
		if p := reg.Providers[name]; databricksUsable(p) {
			return pick(p)
		}
	}
	return RouteInfo{Agent: "copilot", Leg: LegNone,
		Reason: "copilot: " + detail + "; no credit-free route for copilot (no Databricks profile signed in)"}
}

// databricksUsable is true for a Databricks row whose credential resolves.
func databricksUsable(p *providers.Provider) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	_, err := providers.Credential(context.Background(), p)
	return providers.Verdict(err) == providers.VerdictOK
}

// ErrOwnLogin means the vendor should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// CanServe reports whether p is usable by vendor name (providers.Serves without the reason).
func CanServe(name string, p *providers.Provider) bool {
	ok, _ := providers.Serves(name, p, "")
	return ok
}

// fallbackTarget is each vendor's own route to a working credential when the session provider has none.
func fallbackTarget(reg *providers.Registry, name string) (providers.Target, string, bool) {
	switch name {
	case "codex":
		if p, ok := loginProvider(reg, "openai", "codex"); ok && codexChatGPTLogin() == nil {
			// codex's own config.toml model beats the catalog: the ChatGPT account only serves what codex itself lists.
			model := p.Models[models.Opus]
			if own := codexOwnModel(); own != "" {
				model = own
			}
			t, _ := ResolveAgentModel(ModelAsk{Agent: name, Target: providers.Target{Provider: p, Model: model}}) // pin beats both
			return t, "chatgpt (codex login)", true
		}
	case "copilot":
		// copilot's own chain (DescribeRoute): its GitHub login, then BYOK with an anthropic or gemini key.
		if ri := DescribeRoute(reg, "copilot"); ri.OK {
			if t, ok := routeTarget(reg, ri); ok {
				return t, ri.Reason, true
			}
		}
	case "gemini":
		if p, ok := loginProvider(reg, "google", "gemini"); ok && GeminiLoginPresent() {
			// the GEMINI_MODEL pin beats the registry's bare slot id
			t, _ := ResolveAgentModel(ModelAsk{Agent: name, Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
			return t, "gemini (Google login via agy)", true
		}
	case "opencode":
		// opencode's own chain (DescribeRoute): its own GitHub Copilot login, then an anthropic or gemini key.
		if ri := DescribeRoute(reg, "opencode"); ri.OK {
			if t, ok := routeTarget(reg, ri); ok {
				return t, ri.Reason, true
			}
		}
	}
	return providers.Target{}, "", false
}

// keyProvider is the first profile under provider that bills on a harness-held key.
func keyProvider(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.APIKeyAuth() {
			return p, true
		}
	}
	return nil, false
}

// loginProvider is the first profile under provider whose login serves vendor.
func loginProvider(reg *providers.Registry, provider, vendor string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Login == vendor {
			return p, true
		}
	}
	return nil, false
}

// ConfigureHint names the exact command to run when no credential resolved for vendor name at all.
func ConfigureHint(name string) string {
	switch name {
	case "codex":
		return "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key"
	case "copilot":
		return "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key"
	case "opencode":
		return "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)"
	case "gemini":
		return "sign in with `agy` (Google login)"
	default:
		return "run `claude-code configure agents`"
	}
}

// Configure returns extra env (KEY=VALUE) and leading argv that point the vendor CLI at target.
// headlessPrompt != "" asks for the non-interactive form (the runner path).
func Configure(ctx context.Context, name string, target providers.Target, headlessPrompt string) (env, argv []string, err error) {
	if headlessPrompt == "" {
		return configure(ctx, name, target, nil)
	}
	return configure(ctx, name, target, &Headless{Prompt: headlessPrompt})
}

// ConfigureHeadless is Configure's runner form with the per-run options each CLI can carry.
func ConfigureHeadless(ctx context.Context, name string, target providers.Target, h Headless) (env, argv []string, err error) {
	if strings.TrimSpace(h.Prompt) == "" {
		return nil, nil, errors.New("agents: empty prompt")
	}
	return configure(ctx, name, target, &h)
}

// configure resolves the route for name, then applies the vendor's spec launch env/args — on every
// route (own login, BYOK, gateway, router) — so no path can skip it; h != nil (a headless leg) never
// gets Launch.Args, tools-off must stay tools-off and a tools-on leg already carries its own flags.
func configure(ctx context.Context, name string, t providers.Target, h *Headless) ([]string, []string, error) {
	env, argv, err := configureRoute(ctx, name, t, h)
	if err != nil {
		return env, argv, err
	}
	// Route keys go last and shadow same-named launch.env entries, so a spec overlay can never redirect a vendor.
	launchEnv, argv := applyLaunch(name, nil, argv, h == nil)
	return routeEnvLast(launchEnv, env), argv, nil
}

// routeEnvLast is launchEnv minus any key the route env sets, then the route env: the route always wins.
func routeEnvLast(launchEnv, env []string) []string {
	routeKeys := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		routeKeys[k] = true
	}
	var out []string
	for _, kv := range launchEnv {
		if k, _, _ := strings.Cut(kv, "="); !routeKeys[k] {
			out = append(out, kv)
		}
	}
	return append(out, env...)
}

// applyLaunch appends vendor's spec Launch.Env (always) and Launch.Args (only when args is true) to
// an already-built launch; a broken overlay is best-effort and must not break the launch.
func applyLaunch(name string, env, argv []string, args bool) ([]string, []string) {
	if name == "opencode" {
		// Every opencode launch (own login, BYOK, headless) points at the tracked overlay; a failed
		// write is best-effort — opencode still runs on its own global config without it.
		if cfg, tui, err := WriteOpencodeOverlay(); err == nil {
			env = append(env, "OPENCODE_CONFIG="+cfg, "OPENCODE_TUI_CONFIG="+tui)
		}
	}
	vc, err := vendorconfig.Load()
	if err != nil {
		return env, argv
	}
	launch := vc.Vendor(name).Launch
	env = append(env, launch.Env...)
	if args {
		argv = append(argv, launch.Args...)
	}
	return env, argv
}

// LaunchSpec is vendor name's spec env and argv prefix for an interactive launch that bypasses Configure (own login).
func LaunchSpec(name string) (env, argv []string) { return applyLaunch(name, nil, nil, true) }

func configureRoute(ctx context.Context, name string, t providers.Target, h *Headless) ([]string, []string, error) {
	spec, ok := Lookup(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(Names(), "|"))
	}
	p := t.Provider
	if p == nil || t.Model == "" {
		return nil, nil, fmt.Errorf("%s: no provider/model resolved", name)
	}
	if p.Auth.Login != "" {
		return login(name, p, models.Strip1M(t.Model), h)
	}
	if ok, why := providers.Serves(name, p, ""); !ok {
		return nil, nil, fmt.Errorf("%s: %s", name, why)
	}
	dialect, base := "", ""
	for _, d := range spec.Dialects {
		if r, ok := p.Route(d); ok {
			dialect, base = d, r
			break
		}
	}
	model := p.WireModel(models.Strip1M(t.Model)) // idempotent: targets not built by ResolveAgent get the wire id too
	var cred string
	var err error
	routed := false
	if p.Auth.Method() != providers.AuthNone && !isDirect(ctx) {
		// Keyed provider: the vendor gets the router base + router secret; the real credential stays in the router.
		if p, base, cred, err = routerRoute(ctx, spec, p, dialect); err != nil {
			return nil, nil, err
		}
		routed = true
	} else if cred, err = providers.Credential(ctx, p); err != nil {
		return nil, nil, err
	}
	switch name {
	case "codex":
		return codex(p, base, model, cred, h)
	case "opencode":
		return opencode(p, dialect, base, model, cred, routed, h)
	default:
		// The same resolver as the Claude launch decides [1m]; it reads a prefixed wire id (databricks-claude-…) itself.
		client := providers.Target{Provider: t.Provider, Model: t.Model, OneM: t.OneM}.ClientModel()
		return copilot(p, dialect, base, model, providers.ContextWindow(t.Provider, client), cred, routed, h)
	}
}

// Router seams (tests stub these so no daemon is spawned).
var (
	ensureRouter = proxy.Ensure
	routerSecret = proxy.ClientSecret
)

// StubRouterForTest points configure at base for t: an unstubbed Ensure would relaunch the test binary as the daemon.
func StubRouterForTest(t testing.TB, base string) {
	oe := ensureRouter
	t.Cleanup(func() { ensureRouter = oe })
	ensureRouter = func(context.Context) (string, error) { return base, nil }
}

type directKey struct{}

// WithDirect makes configure skip the router and hand the vendor the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func isDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

// routerRoute ensures the router daemon and returns a copy of p routed through it with the router secret as key.
func routerRoute(ctx context.Context, spec Spec, p *providers.Provider, dialect string) (*providers.Provider, string, string, error) {
	rb, err := ensureRouter(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router: %w", spec.Name, err)
	}
	secret, err := routerSecret()
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router secret: %w", spec.Name, err)
	}
	base := proxy.SessionBase(rb, p.Name) + spec.RouterSuffix[dialect]
	rp := *p
	rp.Headers = map[string]string{proxy.AgentHeader: spec.Name} // the router adds the provider's own headers upstream
	return &rp, base, secret, nil
}

// ownedID namespaces the provider entry so it never overwrites a user's own block of the same name.
var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ownedID(p *providers.Provider) (string, error) {
	if !idRE.MatchString(p.Name) {
		return "", fmt.Errorf("provider name %q must match %s to be passed to an agent config", p.Name, idRE)
	}
	return "harness-" + p.Name, nil
}

// codexResponsesModel swaps a model Responses cannot serve (the row's opus slot, gpt-oss) for the menu's first Responses model.
func codexResponsesModel(p *providers.Provider, model string) string {
	gpt := models.FamilyKind(model) == models.FamilyGPT && !models.ChatOnly(model)
	if p.Preset != providers.PresetDatabricks || gpt || providers.ServesResponses(p, model) {
		return model
	}
	return cmp.Or(providers.FirstResponsesModel(p), model)
}

// codex speaks only the Responses API (`wire_api = "chat"` is rejected by current builds).
func codex(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	model = codexResponsesModel(p, model)
	id, err := ownedID(p)
	if err != nil {
		return nil, nil, err
	}
	fields := [][2]string{{"name", tomlStr(p.Name)}, {"base_url", tomlStr(base)}, {"wire_api", tomlStr("responses")}}
	var env []string
	if p.Preset == providers.PresetDatabricks {
		env = append(env, hookspec.CodexProfileEnv+"="+p.Name, hookspec.CodexCatalogEnv+"=")
	}
	if cred != "" {
		env = append(env, CodexKeyEnv+"="+cred)
		if p.Auth.HeaderStyle() == providers.HeaderXAPIKey {
			fields = append(fields, [2]string{"env_http_headers", tomlInline([][2]string{{tomlStr("x-api-key"), tomlStr(CodexKeyEnv)}})})
		} else {
			fields = append(fields, [2]string{"env_key", tomlStr(CodexKeyEnv)})
		}
	}
	if len(p.Headers) > 0 {
		var hs [][2]string
		for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
			hs = append(hs, [2]string{tomlStr(k), tomlStr(p.Headers[k])})
		}
		fields = append(fields, [2]string{"http_headers", tomlInline(hs)})
	}
	argv := []string{"-c", "model_provider=" + tomlStr(id), "-c", "model=" + tomlStr(model),
		"-c", "model_providers." + id + "=" + tomlInline(fields)}
	catalog := codexCatalogArgs(p)
	argv = append(argv, catalog...) // /model offers only ids this row serves
	if len(catalog) == 2 {
		env = append(env, hookspec.CodexCatalogEnv+"="+tomlString(strings.TrimPrefix(catalog[1], "model_catalog_json=")))
	}
	return env, codexHeadless(argv, h), nil
}

// login runs the vendor on its own subscription sign-in; a login provider serves only the CLI it names.
func login(name string, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	switch {
	case name == "opencode" && p.Provider == "github":
		// a github profile's Auth.Login names "copilot", not "opencode": opencode runs on its own GitHub
		// Copilot login (opencode auth login), never the harness's copilot-vendor credential.
		if model == "auto" && p.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
			model = p.OpencodeFallback
		}
		return opencodeCopilotLogin(model, h)
	case p.Auth.Login != name:
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code run %s --provider %s", name, p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	case name == "codex":
		return codexLogin(model, h)
	case name == "copilot":
		return copilotLogin(model, h)
	case name == "gemini":
		return geminiLogin(model, h)
	}
	return nil, nil, fmt.Errorf("%s: no subscription sign-in support", name)
}

// codexLogin runs codex on its own ChatGPT sign-in: the built-in openai provider, no key injected.
func codexLogin(model string, h *Headless) ([]string, []string, error) {
	if err := codexChatGPTLogin(); err != nil {
		return nil, nil, err
	}
	argv := []string{"-c", "model_provider=" + tomlStr("openai"), "-c", "model=" + tomlStr(model)}
	return nil, codexHeadless(argv, h), nil
}

// codexHome is $CODEX_HOME or ~/.codex, where codex keeps auth.json and config.toml.
func codexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".codex")
}

// codexOwnModel is the top-level `model = "..."` in codex's config.toml, or "" (a flat scan; sections are skipped).
func codexOwnModel() string {
	data, err := os.ReadFile(filepath.Join(codexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			return "" // top-level keys end at the first table header
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.Trim(strings.TrimSpace(k), `"'`) == "model" {
			return tomlString(v)
		}
	}
	return ""
}

// tomlString reads one basic or literal TOML string value, dropping a trailing comment; bare values are kept.
func tomlString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
		return strings.Trim(v, `"'`)
	}
	v, _, _ = strings.Cut(v, "#")
	return strings.TrimSpace(v)
}

// codexChatGPTLogin checks the installed CLI's login status, falling back to $CODEX_HOME/auth.json if unavailable.
func codexChatGPTLogin() error {
	if bin, err := paths.VendorBin("codex"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
		if ctx.Err() == nil {
			text := strings.TrimSpace(string(out))
			switch {
			case err == nil && strings.Contains(strings.ToLower(text), "chatgpt"):
				return nil
			case err == nil:
				return fmt.Errorf("codex: signed in with an API key, not ChatGPT (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			default:
				return fmt.Errorf("codex: not signed in (%s); run `codex login` and choose \"Sign in with ChatGPT\"", text)
			}
		}
	}
	b, _, keyringNote := codexAuthJSON()
	var auth struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	if b != nil && json.Unmarshal(b, &auth) == nil && len(auth.Tokens) > 0 && string(auth.Tokens) != "null" {
		return nil
	}
	file := filepath.Join(codexHome(), "auth.json")
	msg := fmt.Sprintf("codex: no ChatGPT sign-in in %s; run `codex login` and choose \"Sign in with ChatGPT\"", file)
	if keyringNote != "" {
		msg += fmt.Sprintf(" (keyring: %s)", keyringNote)
	}
	return errors.New(msg)
}

// codexHeadless appends the `codex exec` form when h is set; nil h is the interactive launch.
func codexHeadless(argv []string, h *Headless) []string {
	var effort string
	if h != nil {
		effort = h.Effort
	}
	if effort = cmp.Or(effort, providers.Pin(providers.PinCodexEffort)); effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+tomlStr(effort))
	}
	if h == nil {
		return argv
	}
	sandbox := "read-only"
	if h.Tools {
		sandbox = "workspace-write"
	}
	argv = append(append([]string{"exec"}, argv...), "--skip-git-repo-check", "--sandbox", sandbox)
	if h.NoPersist {
		argv = append(argv, "--ephemeral")
	}
	if h.SafeMode {
		argv = append(argv, "--ignore-user-config", "--ignore-rules")
	}
	if h.SessionID != "" {
		argv = append(argv, "resume", h.SessionID)
	}
	// The prompt rides stdin behind the `-` operand: a positional prompt like "help" is a codex subcommand.
	return append(argv, "-")
}

// googleHost is Gemini's own API host; a gemini-dialect route anywhere else is a gateway.
const googleHost = "generativelanguage.googleapis.com"

func isGateway(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != googleHost
}

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *Headless) ([]string, []string, error) {
	if !GeminiLoginPresent() {
		return nil, nil, errors.New("gemini: agy is not signed in; " + ConfigureHint("gemini"))
	}
	if h != nil {
		return nil, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// opencode: a generated provider block via OPENCODE_CONFIG_CONTENT; the key is an {env:} reference.
// Interactive runs also list every tier model and import the caller's Claude permissions + MCP servers.
func opencode(p *providers.Provider, dialect, base, model, cred string, routed bool, h *Headless) ([]string, []string, error) {
	id, err := ownedID(p)
	if err != nil {
		return nil, nil, err
	}
	options := map[string]any{"baseURL": base}
	npm := "@ai-sdk/openai-compatible"
	headers := map[string]any{}
	for k, v := range p.Headers {
		headers[k] = v
	}
	ref := "{env:" + OpenCodeKeyEnv + "}"
	// Native SDKs keep provider-specific state (Gemini 3 thought signatures) the openai-compatible shim drops.
	switch dialect {
	case providers.ProtocolAnthropic:
		npm, options["baseURL"] = "@ai-sdk/anthropic", base+"/v1"
	case providers.ProtocolGemini:
		npm, options["baseURL"] = "@ai-sdk/google", base+"/v1beta"
	}
	var env []string
	if cred != "" {
		env = append(env, OpenCodeKeyEnv+"="+cred)
		options["apiKey"] = ref
		native := dialect == providers.ProtocolAnthropic || (dialect == providers.ProtocolGemini && isGateway(base))
		if native && p.Auth.HeaderStyle() == providers.HeaderBearer {
			headers["Authorization"] = "Bearer " + ref
		}
	}
	if len(headers) > 0 {
		options["headers"] = headers
	}
	if p.Preset == providers.PresetDatabricks {
		denv, _, err := opencodeDatabricks(p, id, dialect, base, model, cred, routed, env)
		if err != nil || h == nil {
			return denv, nil, err
		}
		return opencodeHeadlessRun(denv, h, opencodeDatabricksModel(denv))
	}
	menu := opencodeModels(p, model, h)
	if routed && dialect == providers.ProtocolAnthropic {
		for id, m := range menu { // the router cannot relay fine-grained tool streaming
			if entry, ok := m.(map[string]any); ok {
				entry["options"] = map[string]any{"toolStreaming": false}
				menu[id] = entry
			}
		}
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   id + "/" + model,
		"provider": map[string]any{id: map[string]any{
			"npm": npm, "name": p.Name + " (claude-code)", "options": options,
			"models": menu,
		}},
	}
	if h != nil && !h.Tools {
		cfg["permission"] = "deny" // every tool, incl. task subagents and websearch — parity with claude --tools ""
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(body))
	if h == nil {
		return env, nil, nil
	}
	return opencodeHeadlessRun(env, h, id+"/"+model)
}

// opencodeDatabricksModel is the default "model" the databricks split rendered into OPENCODE_CONFIG_CONTENT.
func opencodeDatabricksModel(env []string) string {
	var cfg struct {
		Model string `json:"model"`
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			_ = json.Unmarshal([]byte(v), &cfg)
		}
	}
	return cfg.Model
}

// opencodeHeadlessRun finishes a headless launch: tools-off permission merged into the config env, then the run argv.
func opencodeHeadlessRun(env []string, h *Headless, fullModel string) ([]string, []string, error) {
	if !h.Tools {
		for i, kv := range env {
			v, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT=")
			if !ok {
				continue
			}
			var cfg map[string]any
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				return nil, nil, err
			}
			cfg["permission"] = "deny"
			body, err := json.Marshal(cfg)
			if err != nil {
				return nil, nil, err
			}
			env[i] = "OPENCODE_CONFIG_CONTENT=" + string(body)
		}
	}
	argv := []string{"run", "--format", "json", "-m", fullModel}
	if h.WorkDir != "" {
		argv = append(argv, "--dir", h.WorkDir)
	}
	if h.SessionID != "" {
		argv = append(argv, "--session", h.SessionID)
	}
	if h.Effort != "" {
		argv = append(argv, "--variant", h.Effort)
	}
	if h.Autonomous {
		argv = append(argv, "--auto")
	}
	if h.SafeMode {
		argv = append(argv, "--pure")
	}
	for _, p := range h.Attach {
		argv = append(argv, "-f", p)
	}
	if len(h.Attach) > 0 {
		// -f is variadic: without "--", opencode reads the prompt as another file path.
		return env, append(append(argv, "--"), h.Prompt), nil
	}
	return env, append(argv, positional(h.Prompt)...), nil
}

// opencodeModels is the model menu: the requested model, plus every tier slot for an interactive launch.
func opencodeModels(p *providers.Provider, model string, h *Headless) map[string]any {
	menu := map[string]any{model: map[string]any{"name": model}}
	if h == nil {
		for _, slot := range models.Slots {
			if id := models.Strip1M(p.Models[slot]); id != "" {
				menu[id] = map[string]any{"name": id}
			}
		}
	}
	return menu
}

// OwnLoginHeadless is the headless form of a vendor running on its own stored login/config: argv only, no
// harness route injected. Tools off still rides OPENCODE_CONFIG_CONTENT, which opencode merges over its config.
func OwnLoginHeadless(name string, h Headless) (env, argv []string, err error) {
	if strings.TrimSpace(h.Prompt) == "" {
		return nil, nil, errors.New("agents: empty prompt")
	}
	if name != "opencode" {
		return nil, nil, fmt.Errorf("%s: no headless form on its own login; pass --provider", name)
	}
	env, argv = opencodeRun("", h)
	if cred, err := OpencodeCredential("opencode"); err == nil {
		env = append(env, cred...) // the vault key when opencode stores no login of its own
	}
	launchEnv, argv := applyLaunch("opencode", nil, argv, false) // headless: never Launch.Args
	return routeEnvLast(launchEnv, env), argv, nil
}

// OpencodeRunArgv is opencodeRun exported for the hub runner (internal/agent): one ladder leg execs
// `opencode run` directly on an opencode-native "provider/model" id, bypassing the harness-route config.
func OpencodeRunArgv(model string, h Headless) (env, argv []string) {
	env, argv = opencodeRun(model, h)
	launchEnv, argv := applyLaunch("opencode", nil, argv, false) // headless: never Launch.Args
	return routeEnvLast(launchEnv, env), argv
}

// opencodeRun is `opencode run --format json` for h; a non-empty model adds -m (opencode's provider/model form).
func opencodeRun(model string, h Headless) (env, argv []string) {
	if !h.Tools {
		env = append(env, `OPENCODE_CONFIG_CONTENT={"permission":"deny"}`)
	}
	argv = []string{"run", "--format", "json"}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	if h.WorkDir != "" {
		argv = append(argv, "--dir", h.WorkDir)
	}
	if h.SessionID != "" {
		argv = append(argv, "--session", h.SessionID)
	}
	if h.Effort != "" {
		argv = append(argv, "--variant", h.Effort)
	}
	if h.Autonomous {
		argv = append(argv, "--auto")
	}
	if h.SafeMode {
		argv = append(argv, "--pure")
	}
	for _, p := range h.Attach {
		argv = append(argv, "-f", p)
	}
	if len(h.Attach) > 0 {
		// -f is variadic: without "--", opencode reads the prompt as another file path.
		return env, append(append(argv, "--"), h.Prompt)
	}
	return env, append(argv, positional(h.Prompt)...)
}

// copilot BYOK: COPILOT_PROVIDER_* in the env; COPILOT_OFFLINE keeps it off GitHub's own model path.
// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, or GH_TOKEN); blank BYOK vars cannot redirect it.
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
	if h == nil {
		return env, copilotPinArgv(), nil
	}
	return env, copilotHeadless(h), nil
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

// copilot wires BYOK; window is the model's context window (providers.ContextWindow), which sizes the prompt cap.
func copilot(
	p *providers.Provider, dialect, base, model string, window int, cred string, routed bool, h *Headless,
) ([]string, []string, error) {
	typ := "openai"
	if dialect == providers.ProtocolAnthropic {
		typ = "anthropic"
	}
	wire, modelID := "", ""
	if routed && dialect == providers.ProtocolAnthropic && models.FamilyKind(model) != models.FamilyClaude {
		w, canonical, ok, why := copilotreg.EndpointWire(p, model)
		if !ok {
			return nil, nil, fmt.Errorf("copilot: %s", why)
		}
		if w != providers.ProtocolAnthropic { // the router's openai|responses passthrough
			wire, modelID = w, canonical
			typ, base = "openai", base+"/"+wire
		}
	}
	window, output := copilotreg.Caps(window, model)
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
	if n := copilotreg.PromptTokens(window, output); n > 0 {
		env = append(env, "COPILOT_PROVIDER_MAX_PROMPT_TOKENS="+strconv.Itoa(n))
	}
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
	if h == nil {
		return env, copilotPinArgv(), nil
	}
	return env, copilotHeadless(h), nil
}

func copilotHeadless(h *Headless) []string {
	argv := []string{"--prompt", h.Prompt}
	if h.Tools {
		argv = append(argv, "--allow-all-tools")
	}
	return argv
}

// secretFlagRE matches a CLI flag naming a secret (--key, --api-token=..., -password, ...), case-insensitive.
var secretFlagRE = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(key|token|secret|password)(=.*)?$`)

// RedactConfig blanks mcp.<name>.environment/headers values, provider.<name>.options.headers values,
// secret-flag values in .command, and the query string of .url in an OPENCODE_CONFIG_CONTENT string;
// returns s unchanged on any parse error.
func RedactConfig(s string) string {
	var cfg map[string]any
	if json.Unmarshal([]byte(s), &cfg) != nil {
		return s
	}
	redactMCP(cfg["mcp"])
	redactProviderOptions(cfg["provider"])
	body, err := json.Marshal(cfg)
	if err != nil {
		return s
	}
	return string(body)
}

// redactProviderOptions blanks provider.<name>.options.headers values in the decoded "provider" block;
// options.apiKey is left alone, it is already an "{env:...}" reference, never the literal secret.
func redactProviderOptions(raw any) {
	providers, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, p := range providers {
		entry, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if opts, ok := entry["options"].(map[string]any); ok {
			redactValues(opts["headers"])
		}
	}
}

func redactMCP(raw any) {
	servers, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, s := range servers {
		entry, ok := s.(map[string]any)
		if !ok {
			continue
		}
		redactValues(entry["environment"])
		redactValues(entry["headers"])
		redactCommandSecrets(entry)
		redactURLQuery(entry)
	}
}

// redactValues blanks every value of raw when it is a JSON object; anything else is left alone.
func redactValues(raw any) {
	if m, ok := raw.(map[string]any); ok {
		for k := range m {
			m[k] = "<redacted>"
		}
	}
}

// redactCommandSecrets blanks the value following (or after "=" on) any secret-naming flag in entry["command"].
func redactCommandSecrets(entry map[string]any) {
	argv, ok := entry["command"].([]any)
	if !ok {
		return
	}
	for i := 0; i < len(argv); i++ {
		arg, ok := argv[i].(string)
		if !ok || !secretFlagRE.MatchString(arg) {
			continue
		}
		if idx := strings.IndexByte(arg, '='); idx >= 0 {
			argv[i] = arg[:idx+1] + "<redacted>"
			continue
		}
		if i+1 < len(argv) {
			argv[i+1] = "<redacted>"
			i++
		}
	}
}

// redactURLQuery strips entry["url"]'s query string, appending a marker so a redaction is visible.
func redactURLQuery(entry map[string]any) {
	raw, ok := entry["url"].(string)
	if !ok {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return
	}
	u.RawQuery = ""
	entry["url"] = u.String() + "?<redacted>"
}

// positional guards a dash-led prompt from flag parsing.
func positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}

func tomlStr(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string
	return string(b)
}

func tomlInline(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, kv := range pairs {
		parts[i] = kv[0] + " = " + kv[1]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// CodexChatGPTLoginPresent reports whether codex's own ChatGPT sign-in (~/.codex/auth.json) is on disk.
func CodexChatGPTLoginPresent() bool { return codexChatGPTLogin() == nil }

// CopilotLoginPresent reports a usable GitHub login for copilot: a token env var, the keyring token, its own
// stored login, or gh's active account.
func CopilotLoginPresent() bool {
	_, ok := copilotAuthSource()
	return ok
}

// GeminiLoginPresent reports whether agy's cached Google login is on disk; agy itself owns and refreshes the token.
func GeminiLoginPresent() bool {
	home, _ := os.UserHomeDir()
	for _, p := range agyLoginMarkers(home) {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
}

// AgyLoginMarker is agy's cached Google OAuth token path under home, the marker a login run leaves behind.
// agy 1.4.2 moved this from jetski-standalone-oauth-token to antigravity-cli/antigravity-oauth-token;
// this is the current path a fresh sign-in writes.
func AgyLoginMarker(home string) string {
	return agyLoginMarkers(home)[0]
}

// agyLoginMarkers lists every path agy has used for its cached Google OAuth token, newest first.
func agyLoginMarkers(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"),
	}
}

// opencodeCopilotTarget is opencode's own GitHub Copilot login (`opencode auth login` -> a "github-copilot"
// connection in its stored logins): opus slot, pin-aware. ok is false without that login or a matching registry provider.
func opencodeCopilotTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if !OpencodeCopilotLoginPresent() {
		return providers.Target{}, "", false
	}
	p, ok := loginProvider(reg, "github", "copilot")
	if !ok {
		return providers.Target{}, "", false
	}
	return providers.Target{Provider: p, Model: p.Models[models.Opus]}, "opencode's own GitHub Copilot login (opencode auth login)", true
}

// OpencodeCopilotLoginPresent reports whether opencode's stored logins (OpencodeAuth: 1.x auth.json or the
// 2.x CLI) include "github-copilot", the integration `opencode auth login` stores for a GitHub Copilot sign-in.
func OpencodeCopilotLoginPresent() bool {
	return slices.Contains(OpencodeLogins(), "github-copilot")
}

// opencodeCopilotLogin runs opencode on its own Copilot sign-in: interactive is plain (ErrOwnLogin), headless pins -m.
func opencodeCopilotLogin(model string, h *Headless) ([]string, []string, error) {
	if !OpencodeCopilotLoginPresent() {
		return nil, nil, errors.New("opencode: no GitHub Copilot login in opencode's stored logins; run `opencode auth login` and choose GitHub Copilot")
	}
	if h == nil {
		return nil, nil, ErrOwnLogin
	}
	env, argv := opencodeRun("github-copilot/"+model, *h)
	return env, argv, nil
}

// OpencodeCopilotModelIDs resolves opencode's github-copilot route's sonnet ("model") and haiku
// ("small_model") wire ids: sonnet carries the everyday coding load, haiku is the cheap tier for
// sub-tasks; each honors CLAUDE_<SLOT>_MODEL then CLAUDE_MODEL the way providers.Select resolves a
// session slot, rooted at the github-copilot provider instead of the session's. ok is false without a
// registered github-copilot provider or either slot.
func OpencodeCopilotModelIDs(reg *providers.Registry) (main, small string, ok bool) {
	p, found := loginProvider(reg, "github", "copilot")
	if !found {
		return "", "", false
	}
	main = slotModelID(reg, p, models.Sonnet)
	small = slotModelID(reg, p, models.Haiku)
	return main, small, main != "" && small != ""
}

// slotModelID is slot's pin-resolved wire model id for p: CLAUDE_<SLOT>_MODEL, else CLAUDE_MODEL, else
// p's own table. A pin naming a different provider ("other:model") is respelled the way p serves it. A
// resolved id of "auto" (a router alias opencode's own provider catalog does not list) is swapped for
// p.OpencodeFallback when the registry sets one — config-driven, no model id hardcoded here.
func slotModelID(reg *providers.Registry, p *providers.Provider, slot string) string {
	spec := providers.Pin(providers.SlotPin(slot))
	if spec == "" {
		spec = providers.Pin(providers.PinModel)
	}
	if spec == "" {
		spec = p.Models[slot]
	}
	if spec == "" {
		return ""
	}
	id := p.WireModel(providers.ParseTarget(reg, spec, p).Model) // a cross-provider pin is respelled for p, not copied
	if id == "auto" && p.OpencodeFallback != "" {
		return p.OpencodeFallback
	}
	return id
}
