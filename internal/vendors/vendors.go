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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/models"
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
	{Name: "gemini", Binary: "gemini", Dialect: providers.DialectGemini, Dialects: []string{providers.DialectGemini}},
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
	model := models.Strip1M(t.Model)
	cred, err := providers.Credential(ctx, p)
	if errors.Is(err, providers.ErrPassthrough) && name == "codex" {
		return codexLogin(model, h)
	}
	if errors.Is(err, providers.ErrPassthrough) {
		return nil, nil, fmt.Errorf("%s: provider %s uses passthrough (subscription) auth, which %s cannot use; pick a keyed provider", name, p.Name, name)
	}
	if err != nil {
		return nil, nil, err
	}
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

// codexLogin runs codex on its own ChatGPT sign-in: the built-in openai provider, no key injected.
func codexLogin(model string, h *Headless) ([]string, []string, error) {
	if err := codexChatGPTLogin(); err != nil {
		return nil, nil, err
	}
	argv := []string{"-c", "model_provider=" + tomlStr("openai"), "-c", "model=" + tomlStr(model)}
	return nil, codexHeadless(argv, h), nil
}

// codexChatGPTLogin fails fast unless $CODEX_HOME/auth.json holds ChatGPT tokens (`codex login` writes them).
func codexChatGPTLogin() error {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		dir, _ := os.UserHomeDir()
		home = filepath.Join(dir, ".codex")
	}
	file := filepath.Join(home, "auth.json")
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
	return append(argv, positional(h.Prompt)...)
}

// googleHost is Gemini's own API host; any other gemini route is a gateway.
const googleHost = "generativelanguage.googleapis.com"

func isGateway(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != googleHost
}

// gemini: Google's own host takes the bare key; any other base is a gateway and needs the base URL too.
// Every knob is set explicitly (empty clears) so an inherited GOOGLE_*/GEMINI_* var cannot redirect the run.
// Interactive runs also trust the workspace (GEMINI_CLI_TRUST_WORKSPACE), equivalent to --skip-trust.
func gemini(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	if err := geminiAuthConflict(); err != nil {
		return nil, nil, err
	}
	baseURL, mechanism := "", "x-goog-api-key"
	if isGateway(base) {
		baseURL = base
		if p.Auth.Type == providers.AuthBearer {
			mechanism = "bearer"
		}
	}
	var hs []string
	for _, k := range slices.Sorted(maps.Keys(p.Headers)) {
		hs = append(hs, k+":"+p.Headers[k])
	}
	env := []string{
		"GEMINI_MODEL=" + model,
		"GEMINI_API_KEY=" + cred,
		"GOOGLE_GEMINI_BASE_URL=" + baseURL,
		"GEMINI_API_KEY_AUTH_MECHANISM=" + mechanism,
		"GEMINI_CLI_CUSTOM_HEADERS=" + strings.Join(hs, ","),
		"GOOGLE_GENAI_USE_GCA=",
		"GOOGLE_GENAI_USE_VERTEXAI=",
	}
	if h == nil {
		return append(env, "GEMINI_CLI_TRUST_WORKSPACE=true"), nil, nil
	}
	approval := "plan" // read-only, like codex's read-only sandbox
	if h.Tools {
		approval = "yolo"
	}
	argv := []string{"--skip-trust", "-m", model, "--output-format", "stream-json", "--approval-mode", approval}
	if h.SessionID != "" {
		argv = append(argv, "--resume", h.SessionID)
	}
	return env, append(argv, "--prompt="+h.Prompt), nil
}

// geminiKeyAuth are the selectedType values that read GEMINI_API_KEY; any other one ignores the harness wiring.
var geminiKeyAuth = map[string]bool{"": true, "gemini-api-key": true, "gateway": true}

// geminiAuthConflict fails fast when ~/.gemini/settings.json picks a login auth: it beats every env var, and
// a headless run would hang on the browser prompt (a system override file must be root-owned, so none is possible).
func geminiAuthConflict() error {
	home := os.Getenv("GEMINI_CLI_HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	file := filepath.Join(home, ".gemini", "settings.json")
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var cfg struct {
		Legacy   string `json:"selectedAuthType"`
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil // gemini-cli reports its own parse errors
	}
	if t := cmp.Or(cfg.Security.Auth.SelectedType, cfg.Legacy); !geminiKeyAuth[t] {
		return fmt.Errorf("gemini: %s selects auth %q, which overrides the provider key; run `gemini`, /auth → "+
			"\"Use Gemini API key\", or delete security.auth.selectedType", file, t)
	}
	return nil
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

// copilotMaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const copilotMaxOutputTokens = 32_000

// copilot BYOK: COPILOT_PROVIDER_* in the env; COPILOT_OFFLINE keeps it off GitHub's own model path.
func copilot(p *providers.Provider, dialect, base, model, cred string, h *Headless) ([]string, []string, error) {
	typ := "openai"
	if dialect == providers.DialectAnthropic {
		typ = "anthropic"
	}
	env := []string{
		"COPILOT_PROVIDER_BASE_URL=" + base,
		"COPILOT_PROVIDER_TYPE=" + typ,
		"COPILOT_MODEL=" + model,
		"COPILOT_PROVIDER_MAX_PROMPT_TOKENS=" + strconv.Itoa(models.ContextWindow(model)),
		"COPILOT_PROVIDER_MAX_OUTPUT_TOKENS=" + strconv.Itoa(copilotMaxOutputTokens),
		"COPILOT_OFFLINE=true",
	}
	if cred != "" {
		env = append(env, "COPILOT_PROVIDER_API_KEY="+cred)
		if p.Auth.Type == providers.AuthBearer {
			env = append(env, "COPILOT_PROVIDER_BEARER_TOKEN="+cred)
		}
	}
	if h == nil {
		return env, nil, nil
	}
	argv := []string{"--prompt", h.Prompt}
	if h.Tools {
		argv = append(argv, "--allow-all-tools")
	}
	return env, argv, nil
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
