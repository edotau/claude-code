// Package vendors wires third-party agent CLIs (codex, gemini, opencode, copilot) to a provider target,
// shared by the interactive `claude-code run` launch and the headless agent runners.
package vendors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
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
	{Name: "opencode", Binary: "opencode", Dialect: providers.DialectAnthropic, Dialects: []string{providers.DialectAnthropic, providers.DialectOpenAI}},
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
	cred, err := providers.Credential(ctx, p)
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
		for _, k := range sortedKeys(p.Headers) {
			hs = append(hs, [2]string{tomlStr(k), tomlStr(p.Headers[k])})
		}
		fields = append(fields, [2]string{"http_headers", tomlInline(hs)})
	}
	argv := []string{"-c", "model_provider=" + tomlStr(id), "-c", "model=" + tomlStr(model),
		"-c", "model_providers." + id + "=" + tomlInline(fields)}
	if h == nil {
		return env, argv, nil
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
	return env, append(argv, positional(h.Prompt)...), nil
}

// gemini: Google's own host takes the bare key; any other base is a gateway and needs the base URL too.
func gemini(p *providers.Provider, base, model, cred string, h *Headless) ([]string, []string, error) {
	env := []string{"GEMINI_MODEL=" + model}
	if cred != "" {
		env = append(env, "GEMINI_API_KEY="+cred)
	}
	if u, err := url.Parse(base); err == nil && u.Host != "generativelanguage.googleapis.com" {
		env = append(env, "GOOGLE_GEMINI_BASE_URL="+base)
		if p.Auth.Type == providers.AuthBearer {
			env = append(env, "GEMINI_API_KEY_AUTH_MECHANISM=bearer")
		}
	}
	if len(p.Headers) > 0 {
		var hs []string
		for _, k := range sortedKeys(p.Headers) {
			hs = append(hs, k+":"+p.Headers[k])
		}
		env = append(env, "GEMINI_CLI_CUSTOM_HEADERS="+strings.Join(hs, ","))
	}
	if h == nil {
		return env, nil, nil
	}
	return env, []string{"-m", model, "-p", h.Prompt}, nil
}

// opencode: a generated provider block via OPENCODE_CONFIG_CONTENT; the key is an {env:} reference.
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
	if dialect == providers.DialectAnthropic {
		npm, options["baseURL"] = "@ai-sdk/anthropic", base+"/v1"
	}
	var env []string
	if cred != "" {
		env = append(env, OpenCodeKeyEnv+"="+cred)
		options["apiKey"] = ref
		if dialect == providers.DialectAnthropic && p.Auth.Type == providers.AuthBearer {
			headers["Authorization"] = "Bearer " + ref
		}
	}
	if len(headers) > 0 {
		options["headers"] = headers
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   id + "/" + model,
		"provider": map[string]any{id: map[string]any{
			"npm": npm, "name": p.Name + " (claude-code)", "options": options,
			"models": map[string]any{model: map[string]any{"name": model}},
		}},
	}
	if h != nil && !h.Tools {
		cfg["permission"] = map[string]any{"edit": "deny", "bash": "deny", "webfetch": "deny"}
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(body))
	if h == nil {
		return env, nil, nil
	}
	argv := []string{"run", "-m", id + "/" + model}
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
