// Package vendors wires third-party agent CLIs (codex, gemini, opencode, copilot) to a provider target,
// shared by the interactive `claude-code run` launch and the headless agent runners.
package vendors

import (
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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// Spec describes one vendor CLI.
type Spec struct {
	Name     string   // codex | gemini | opencode | copilot
	Binary   string   // executable name on PATH
	Dialect  string   // preferred provider route (providers.Dialect*)
	Dialects []string // every route the CLI can speak, preferred first
}

var specs = []Spec{
	{Name: "codex", Binary: "codex", Dialect: providers.DialectResponses, Dialects: []string{providers.DialectResponses}},
	{Name: "gemini", Binary: "agy", Dialect: providers.DialectGemini, Dialects: []string{providers.DialectGemini}},
	{Name: "opencode", Binary: "opencode", Dialect: providers.DialectAnthropic, Dialects: []string{providers.DialectAnthropic, providers.DialectGemini, providers.DialectOpenAI}},
	{Name: "copilot", Binary: "copilot", Dialect: providers.DialectAnthropic, Dialects: []string{providers.DialectAnthropic, providers.DialectOpenAI}},
}

// Names lists the vendors in roster order.
func Names() []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// Lookup returns the vendor spec by name.
func Lookup(name string) (Spec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// Headless is the non-interactive form of a run; zero Tools means tools are disabled where the CLI allows it.
type Headless struct {
	Prompt    string
	Effort    string
	SessionID string // resume this session
	Tools     bool
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
		if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
			return gt, gnote, nil
		}
	}
	if explicit || canServe(name, t.Provider) {
		return t, "", nil
	}
	if ft, fallbackNote, ok := fallbackTarget(reg, name); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
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

// ErrOwnLogin means the vendor should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("vendor runs on its own login")

// canServe reports whether p is usable by vendor name: a login provider naming it, or a route on one
// of the dialects the vendor speaks.
func canServe(name string, p *providers.Provider) bool {
	// A passthrough provider carries a login only its own vendor can forward; a dialect match is not enough.
	if p.Auth.Login != "" || p.Auth.Type == "passthrough" {
		return p.Auth.Login == name
	}
	spec, ok := Lookup(name)
	if !ok {
		return false
	}
	for _, d := range spec.Dialects {
		if _, ok := p.Route(d); ok {
			return true
		}
	}
	return false
}

// fallbackTarget is each vendor's own route to a working credential when the session provider has none.
func fallbackTarget(reg *providers.Registry, name string) (providers.Target, string, bool) {
	switch name {
	case "codex":
		if p, ok := loginProvider(reg, "chatgpt", "codex"); ok && codexChatGPTLogin() == nil {
			// codex's own config.toml model beats the catalog: the ChatGPT account only serves what codex itself lists.
			model := p.Models[models.Opus]
			if own := codexOwnModel(); own != "" {
				model = own
			}
			return providers.Target{Provider: p, Model: model}, "chatgpt (codex login)", true
		}
	case "copilot":
		// copilot's own chain (DescribeRoute): its GitHub login, then BYOK with an anthropic or gemini key.
		if ri := DescribeRoute(reg, "copilot"); ri.OK {
			if t, ok := routeTarget(reg, ri); ok {
				return t, ri.Reason, true
			}
		}
	case "gemini":
		if p, ok := reg.Providers["gemini"]; ok && GeminiLoginPresent() {
			return providers.Target{Provider: p, Model: p.Models[models.Opus]}, "gemini (Google login via agy)", true
		}
		if p, ok := reg.Providers["gemini"]; ok && providers.CredentialPresent(p) {
			return providers.Target{Provider: p, Model: p.Models[models.Opus]}, "gemini (GEMINI_API_KEY)", true
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

func loginProvider(reg *providers.Registry, providerName, vendor string) (*providers.Provider, bool) {
	p, ok := reg.Providers[providerName]
	if !ok || p.Auth.Login != vendor {
		return nil, false
	}
	return p, true
}

// copilotLoginPresent checks GH_TOKEN first, then `gh auth token` (copilot's own sign-in surface) — the
// offline form: `gh auth status` validates over the network and blew its 2 s budget, dropping the fallback.
// copilotLoginPresent: copilot's token precedence is COPILOT_GITHUB_TOKEN, GH_TOKEN, GITHUB_TOKEN, then `gh`.
func copilotLoginPresent() bool {
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "gh", "auth", "token").Run() == nil
}

// ConfigureHint names the exact command to run when no credential resolved for vendor name at all.
func ConfigureHint(name string) string {
	switch name {
	case "codex":
		return "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key"
	case "copilot", "opencode":
		return "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key"
	case "gemini":
		return "sign in with `agy`, or `claude-code configure` → gemini → API key"
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
		return nil, nil, errors.New("vendors: empty prompt")
	}
	return configure(ctx, name, target, &h)
}

func configure(ctx context.Context, name string, t providers.Target, h *Headless) ([]string, []string, error) {
	spec, ok := Lookup(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown vendor %q (%s)", name, strings.Join(Names(), "|"))
	}
	p := t.Provider
	if p == nil || t.Model == "" {
		return nil, nil, fmt.Errorf("%s: no provider/model resolved", name)
	}
	if p.Auth.Login != "" {
		return login(name, p, models.Strip1M(t.Model), h)
	}
	dialect, base := "", ""
	for _, d := range spec.Dialects {
		if r, ok := p.Route(d); ok {
			dialect, base = d, r
			break
		}
	}
	if dialect == "" {
		return nil, nil, fmt.Errorf("%s: provider %s serves no %s route (it has %s)", name, p.Name,
			strings.Join(spec.Dialects, "/"), strings.Join(p.Dialects(), ","))
	}
	cred, err := providers.Credential(ctx, p)
	if name == "gemini" && GeminiLoginPresent() {
		cred, err = "", nil // agy's cached Google login wins over any key
	}
	if errors.Is(err, providers.ErrPassthrough) {
		return nil, nil, fmt.Errorf("%s: provider %s uses passthrough (subscription) auth, which %s cannot use; pick a keyed provider", name, p.Name, name)
	}
	if err != nil {
		return nil, nil, err
	}
	model := models.Strip1M(t.Model)
	switch name {
	case "codex":
		return codex(p, base, model, cred, h)
	case "gemini":
		return gemini(p, base, model, cred, h)
	case "opencode":
		return opencode(p, dialect, base, model, cred, h)
	default:
		return copilot(p, dialect, base, model, cred, h)
	}
}

// ownedID namespaces the provider entry so it never overwrites a user's own block of the same name.
var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func ownedID(p *providers.Provider) (string, error) {
	if !idRE.MatchString(p.Name) {
		return "", fmt.Errorf("provider name %q must match %s to be passed to a vendor config", p.Name, idRE)
	}
	return "harness-" + p.Name, nil
}

// codex speaks only the Responses API (`wire_api = "chat"` is rejected by current builds).
func codex(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	id, err := ownedID(p)
	if err != nil {
		return nil, nil, err
	}
	fields := [][2]string{{"name", tomlStr(p.Name)}, {"base_url", tomlStr(base)}, {"wire_api", tomlStr("responses")}}
	var env []string
	if cred != "" {
		env = append(env, CodexKeyEnv+"="+cred)
		if p.Auth.Type == providers.AuthAPIKey {
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
	return env, codexHeadless(argv, h), nil
}

// login runs the vendor on its own subscription sign-in; a login provider serves only the CLI it names.
func login(name string, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	switch {
	case name == "opencode" && p.Name == "github-copilot":
		// github-copilot's Auth.Login names "copilot", not "opencode": opencode runs on its own GitHub
		// Copilot login (opencode auth login), never the harness's copilot-vendor credential.
		return opencodeCopilotLogin(model, h)
	case p.Auth.Login != name:
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code run %s --provider %s", name, p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	case name == "codex":
		return codexLogin(model, h)
	case name == "copilot":
		return copilotLogin(model, h)
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
	for _, line := range strings.Split(string(data), "\n") {
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

// codexChatGPTLogin fails fast unless codex reports a ChatGPT sign-in. `codex login status` is authoritative (it
// also sees keyring-stored logins); $CODEX_HOME/auth.json is the fallback when the binary is not on PATH.
func codexChatGPTLogin() error {
	if bin, err := paths.LookPathReal("codex"); err == nil {
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
	file := filepath.Join(codexHome(), "auth.json")
	var auth struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	b, err := os.ReadFile(file)
	if err == nil {
		err = json.Unmarshal(b, &auth)
	}
	if err != nil || len(auth.Tokens) == 0 || string(auth.Tokens) == "null" {
		return fmt.Errorf("codex: no ChatGPT sign-in in %s; run `codex login` and choose \"Sign in with ChatGPT\"", file)
	}
	return nil
}

// codexHeadless appends the `codex exec` form when h is set; nil h is the interactive launch.
func codexHeadless(argv []string, h *Headless) []string {
	if h == nil {
		return argv
	}
	if h.Effort != "" {
		argv = append(argv, "-c", "model_reasoning_effort="+tomlStr(h.Effort))
	}
	sandbox := "read-only"
	if h.Tools {
		sandbox = "workspace-write"
	}
	argv = append(append([]string{"exec"}, argv...), "--skip-git-repo-check", "--sandbox", sandbox)
	if h.SessionID != "" {
		argv = append(argv, "resume", h.SessionID)
	}
	// The prompt rides stdin behind the `-` operand: a positional prompt like "help" is a codex subcommand.
	return append(argv, "-")
}

// googleHost is Gemini's own API host; any other gemini route is a gateway.
const googleHost = "generativelanguage.googleapis.com"

func isGateway(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != googleHost
}

// gemini: agy speaks only Google's own backend — a gateway base (anything but googleHost) is a hard error.
// A cached agy login (AgyLoginMarker) always wins over GEMINI_API_KEY, so the key rides only without one.
func gemini(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	if isGateway(base) {
		return nil, nil, errors.New("agy has no custom base URL; gemini runs on Google's backend only")
	}
	var env []string
	if !GeminiLoginPresent() {
		env = append(env, "GEMINI_API_KEY="+cred)
	}
	if h != nil {
		return env, nil, nil // headless argv is agent.RunAgy's (prompt over stdin stream-json)
	}
	return env, []string{"--model", model}, nil
}

// opencode: a generated provider block via OPENCODE_CONFIG_CONTENT; the key is an {env:} reference.
// Interactive runs also list every tier model and import the caller's Claude permissions + MCP servers.
func opencode(p *providers.Provider, dialect, base, model, cred string, h *Headless) ([]string, []string, error) {
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
	case providers.DialectAnthropic:
		npm, options["baseURL"] = "@ai-sdk/anthropic", base+"/v1"
	case providers.DialectGemini:
		npm, options["baseURL"] = "@ai-sdk/google", base+"/v1beta"
	}
	var env []string
	if cred != "" {
		env = append(env, OpenCodeKeyEnv+"="+cred)
		options["apiKey"] = ref
		native := dialect == providers.DialectAnthropic || (dialect == providers.DialectGemini && isGateway(base))
		if native && p.Auth.Type == providers.AuthBearer {
			headers["Authorization"] = "Bearer " + ref
		}
	}
	if len(headers) > 0 {
		options["headers"] = headers
	}
	menu := map[string]any{model: map[string]any{"name": model}}
	if h == nil {
		for _, slot := range models.Slots {
			if id := models.Strip1M(p.Models[slot]); id != "" {
				menu[id] = map[string]any{"name": id}
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
	if h == nil {
		cwd, _ := os.Getwd()
		imp := readClaudeImport(cwd)
		if perm := permissionBlock(imp.allow, imp.deny, imp.ask); perm != nil {
			cfg["permission"] = perm
		}
		if mcp := mcpBlock(imp.mcp); len(mcp) > 0 {
			cfg["mcp"] = mcp
		}
		if len(imp.skippedMCP) > 0 {
			env = append(env, "HARNESS_OPENCODE_MCP_SKIPPED="+strings.Join(imp.skippedMCP, ","))
		}
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
	argv := []string{"run", "--format", "json", "-m", id + "/" + model}
	if h.SessionID != "" {
		argv = append(argv, "--session", h.SessionID)
	}
	if h.Effort != "" {
		argv = append(argv, "--variant", h.Effort)
	}
	return env, append(argv, positional(h.Prompt)...), nil
}

// OwnLoginHeadless is the headless form of a vendor running on its own stored login/config: argv only, no
// harness route injected. Tools off still rides OPENCODE_CONFIG_CONTENT, which opencode merges over its config.
func OwnLoginHeadless(name string, h Headless) (env, argv []string, err error) {
	if strings.TrimSpace(h.Prompt) == "" {
		return nil, nil, errors.New("vendors: empty prompt")
	}
	if name != "opencode" {
		return nil, nil, fmt.Errorf("%s: no headless form on its own login; pass --provider", name)
	}
	env, argv = opencodeRun("", h)
	return env, argv, nil
}

// OpencodeRunArgv is opencodeRun exported for the hub runner (internal/agent): one ladder leg execs
// `opencode run` directly on an opencode-native "provider/model" id, bypassing the harness-route config.
func OpencodeRunArgv(model string, h Headless) (env, argv []string) { return opencodeRun(model, h) }

// opencodeRun is `opencode run --format json` for h; a non-empty model adds -m (opencode's provider/model form).
func opencodeRun(model string, h Headless) (env, argv []string) {
	if !h.Tools {
		env = append(env, `OPENCODE_CONFIG_CONTENT={"permission":"deny"}`)
	}
	argv = []string{"run", "--format", "json"}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	if h.SessionID != "" {
		argv = append(argv, "--session", h.SessionID)
	}
	if h.Effort != "" {
		argv = append(argv, "--variant", h.Effort)
	}
	return env, append(argv, positional(h.Prompt)...)
}

// copilotMaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const copilotMaxOutputTokens = 32_000

// copilot BYOK: COPILOT_PROVIDER_* in the env; COPILOT_OFFLINE keeps it off GitHub's own model path.
// copilotLogin runs copilot on its GitHub sign-in (`copilot login`, or GH_TOKEN); blank BYOK vars cannot redirect it.
func copilotLogin(model string, h *Headless) ([]string, []string, error) {
	env := []string{"COPILOT_MODEL=" + model, "COPILOT_PROVIDER_BASE_URL=", "COPILOT_OFFLINE="}
	if h == nil {
		return env, nil, nil
	}
	return env, copilotHeadless(h), nil
}

func copilot(p *providers.Provider, dialect, base, model, cred string, h *Headless) ([]string, []string, error) {
	typ := "openai"
	if dialect == providers.DialectAnthropic {
		typ = "anthropic"
	}
	env := []string{
		"COPILOT_PROVIDER_BASE_URL=" + base,
		"COPILOT_PROVIDER_TYPE=" + typ,
		"COPILOT_MODEL=" + model,
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=" + strconv.Itoa(copilotMaxOutputTokens),
		"COPILOT_OFFLINE=true",
		// Blank every BYOK credential var so a token exported in the parent shell cannot outrank this route.
		"COPILOT_PROVIDER_API_KEY=", "COPILOT_PROVIDER_BEARER_TOKEN=", "COPILOT_PROVIDER_API_KEY_COMMAND=",
	}
	// Prompt budget = window minus the output cap, so prompt + output never overshoot the model's context.
	if w := models.ContextWindow(model); w > copilotMaxOutputTokens {
		env = append(env, "COPILOT_PROVIDER_MAX_PROMPT_TOKENS="+strconv.Itoa(w-copilotMaxOutputTokens))
	}
	if cred != "" {
		// Bearer precedence is copilot's own rule; sending the key on both headers trips strict gateways.
		if p.Auth.Type == providers.AuthBearer {
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
		return env, nil, nil
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

// RedactConfig blanks mcp.<name>.environment/headers values, secret-flag values in .command, and the
// query string of .url in an OPENCODE_CONFIG_CONTENT string; returns s unchanged on any parse error.
func RedactConfig(s string) string {
	var cfg map[string]any
	if json.Unmarshal([]byte(s), &cfg) != nil {
		return s
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return s
	}
	for _, raw := range mcp {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"environment", "headers"} {
			if m, ok := entry[key].(map[string]any); ok {
				for k := range m {
					m[k] = "<redacted>"
				}
			}
		}
		redactCommandSecrets(entry)
		redactURLQuery(entry)
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return s
	}
	return string(body)
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

// CopilotLoginPresent reports a usable GitHub login for copilot: GH_TOKEN or an authenticated `gh`.
func CopilotLoginPresent() bool { return copilotLoginPresent() }

// GeminiLoginPresent reports whether agy's cached Google login is on disk; it beats GEMINI_API_KEY (even a bogus key).
func GeminiLoginPresent() bool {
	home, _ := os.UserHomeDir()
	_, err := os.Stat(AgyLoginMarker(home))
	return err == nil
}

// AgyLoginMarker is agy's cached Google OAuth token path under home, the marker a login run leaves behind.
func AgyLoginMarker(home string) string {
	return filepath.Join(home, ".gemini", "jetski-standalone-oauth-token")
}

// opencodeCopilotTarget is opencode's own GitHub Copilot login (`opencode auth login` -> the "github-copilot"
// key in its auth.json): opus slot, pin-aware. ok is false without that login or a matching registry provider.
func opencodeCopilotTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if !OpencodeCopilotLoginPresent() {
		return providers.Target{}, "", false
	}
	p, ok := loginProvider(reg, "github-copilot", "copilot")
	if !ok {
		return providers.Target{}, "", false
	}
	return providers.Target{Provider: p, Model: p.Models[models.Opus]}, "opencode's own GitHub Copilot login (opencode auth login)", true
}

// OpencodeCopilotLoginPresent reports whether opencode's own auth.json has a "github-copilot" entry,
// the key `opencode auth login` writes for a GitHub Copilot sign-in.
func OpencodeCopilotLoginPresent() bool {
	return slices.Contains(OpencodeLogins(), "github-copilot")
}

// opencodeCopilotLogin runs opencode on its own Copilot sign-in: interactive is plain (ErrOwnLogin), headless pins -m.
func opencodeCopilotLogin(model string, h *Headless) ([]string, []string, error) {
	if !OpencodeCopilotLoginPresent() {
		return nil, nil, errors.New("opencode: no GitHub Copilot login in opencode's own auth.json; run `opencode auth login` and choose GitHub Copilot")
	}
	if h == nil {
		return nil, nil, ErrOwnLogin
	}
	env, argv := opencodeRun("github-copilot/"+model, *h)
	return env, argv, nil
}

// OpencodeCopilotModelIDs resolves opencode's github-copilot route's sonnet ("model") and haiku
// ("small_model") wire ids: sonnet carries the everyday coding load, haiku is the cheap tier for
// sub-tasks; each honors HARNESS_<SLOT>_MODEL then HARNESS_MODEL the way providers.Select resolves a
// session slot, rooted at the github-copilot provider instead of the session's. ok is false without a
// registered github-copilot provider or either slot.
func OpencodeCopilotModelIDs(reg *providers.Registry) (main, small string, ok bool) {
	p, found := reg.Providers["github-copilot"]
	if !found {
		return "", "", false
	}
	main = slotModelID(reg, p, models.Sonnet)
	small = slotModelID(reg, p, models.Haiku)
	return main, small, main != "" && small != ""
}

// slotModelID is slot's pin-resolved wire model id for p: HARNESS_<SLOT>_MODEL, else HARNESS_MODEL, else
// p's own table. A pin naming a different provider ("other:model") is still used verbatim as the id. A
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
	id := providers.ParseTarget(reg, spec, p).Model
	if id == "auto" && p.OpencodeFallback != "" {
		return p.OpencodeFallback
	}
	return id
}

// OpenCodePermissionMCP renders the permission/mcp blocks opencode's interactive config normally imports
// from the caller's Claude settings (cwd-rooted) — reused by render paths in internal/generate that build
// their own opencode.json directly instead of going through Configure/opencode (the github-copilot route,
// which Configure cannot drive: a login provider's Auth.Login names "copilot", not "opencode").
func OpenCodePermissionMCP() (permission map[string]any, mcp map[string]any, mcpSkipped []string) {
	cwd, _ := os.Getwd()
	imp := readClaudeImport(cwd)
	return permissionBlock(imp.allow, imp.deny, imp.ask), mcpBlock(imp.mcp), imp.skippedMCP
}

// OpencodeLogins lists the providers opencode's own `auth login` stored (auth.json under its data dir), sorted.
func OpencodeLogins() []string {
	data, err := os.ReadFile(opencodeAuthFile())
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// opencodeAuthFile is $XDG_DATA_HOME/opencode/auth.json (default ~/.local/share), where `opencode auth login` writes.
func opencodeAuthFile() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "auth.json")
}
