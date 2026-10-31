package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

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
	env = append(env, opencodeConfigEnv+"="+string(body))
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
		if v, ok := strings.CutPrefix(kv, opencodeConfigEnv+"="); ok {
			_ = json.Unmarshal([]byte(v), &cfg)
		}
	}
	return cfg.Model
}

// opencodeHeadlessRun finishes a headless launch: tools-off permission merged into the config env, then the run argv.
func opencodeHeadlessRun(env []string, h *Headless, fullModel string) ([]string, []string, error) {
	if !h.Tools {
		var err error
		if env, err = mutateOpencodeConfigEnv(env, false, func(cfg map[string]any) { cfg["permission"] = "deny" }); err != nil {
			return nil, nil, err
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

// opencodeRunArgv is opencodeRun exported for the hub runner (internal/agent): one ladder leg execs
// `opencode run` directly on an opencode-native "provider/model" id, bypassing the harness-route config.
func opencodeRunArgv(model string, h Headless) (env, argv []string) {
	env, argv = opencodeRun(model, h)
	launchEnv, argv := schemaFor("opencode").applyLaunch(nil, argv, false) // headless: never Launch.Args
	return routeEnvLast(launchEnv, env), argv
}

// opencodeRun is `opencode run --format json` for h; a non-empty model adds -m (opencode's provider/model form).
func opencodeRun(model string, h Headless) (env, argv []string) {
	if !h.Tools {
		env = append(env, opencodeConfigEnv+`={"permission":"deny"}`)
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

// opencodeConfigEnv is the env var carrying opencode's inline JSON config.
const opencodeConfigEnv = "OPENCODE_CONFIG_CONTENT"

// mutateOpencodeConfigEnv applies fn to the JSON in each OpencodeConfigEnv entry of env (appending one when absent).
// An unreadable block is an error, or with lenient replaced by an empty config.
func mutateOpencodeConfigEnv(env []string, lenient bool, fn func(map[string]any)) ([]string, error) {
	found := false
	for i, kv := range env {
		body, ok := strings.CutPrefix(kv, opencodeConfigEnv+"=")
		if !ok {
			continue
		}
		found = true
		var cfg map[string]any
		if err := json.Unmarshal([]byte(body), &cfg); err != nil && !lenient {
			return nil, err
		}
		if cfg == nil { // unreadable (lenient) or JSON null
			cfg = map[string]any{}
		}
		fn(cfg)
		out, err := json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
		env[i] = opencodeConfigEnv + "=" + string(out)
	}
	if found {
		return env, nil
	}
	cfg := map[string]any{}
	fn(cfg)
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append(env, opencodeConfigEnv+"="+string(out)), nil
}

// opencodeOverlayModels is the overlay's model/small_model: github-copilot/ ids on the copilot-only leg (no
// opencode-go login, so the table's opencode-go cells would be unservable), else the table's opencode cells.
func opencodeOverlayModels() (mainModel, smallModel string, ok bool) {
	if !OpencodeOwnLoginPresent() && OpencodeCopilotLoginPresent() {
		reg, _ := providers.Load()
		if m, s, found := OpencodeCopilotModelIDs(reg); found {
			return "github-copilot/" + m, "github-copilot/" + s, true
		}
	}
	return OpencodeTableModels()
}

type opencodeEngine struct{ baseEngine }

var _ headlessEngine = opencodeEngine{}

func (opencodeEngine) after(p *cliPrep, req RunOptions) {
	if req.SystemPrompt != "" {
		p.env, p.argv = opencodeSystemPrompt(p.env, p.argv, req.SystemPrompt)
	}
}

func (e opencodeEngine) exec(ctx context.Context, p cliPrep, req RunOptions, sink Sink) (Result, error) {
	bin, err := e.bin()
	if err != nil {
		return p.res, err
	}
	cmd, tail := childCmd(ctx, bin, p.argv, p.env, req.WorkDir, "", req.KeepEnv, true)
	return runStream(ctx, e.name, cmd, tail, p.res, sink, parseOpenCodeStream)
}

// loginServes adds github: that profile's Auth.Login names copilot, yet opencode has its own GitHub Copilot login.
func (e opencodeEngine) loginServes(p *providers.Provider) bool {
	return p.Provider == "github" || e.baseEngine.loginServes(p)
}

func (opencodeEngine) login(p *providers.Provider, model string, h *Headless) (env, argv []string, err error) {
	if p.Provider != "github" {
		return nil, nil, errors.New("opencode: no subscription sign-in support")
	}
	if model == "auto" && p.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
		model = p.OpencodeFallback
	}
	return opencodeCopilotLogin(model, h)
}

func (opencodeEngine) wire(ctx context.Context, r wireRoute, h *Headless) (env, argv []string, err error) {
	return opencode(r.Provider, r.Dialect, r.Base, r.Model, r.Cred, r.Routed, h)
}

func (e opencodeEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return routeFallback(reg, e.describeRoute(reg))
}

func (opencodeEngine) describeRoute(reg *providers.Registry) RouteInfo {
	return describeOpenCodeRoute(reg)
}

func (opencodeEngine) LoginStatus() (bool, string) {
	if names := OpencodeLogins(); len(names) > 0 {
		return true, "opencode auth login: " + strings.Join(names, ", ")
	}
	return false, ""
}

// preRoute prefers opencode's own login (Go/Zen, else the github-copilot one) ahead of a session provider that could serve it.
func (opencodeEngine) preRoute(reg *providers.Registry, explicit bool) (providers.Target, string, bool, error) {
	if explicit {
		return providers.Target{}, "", false, nil
	}
	if OpencodeOwnLoginPresent() { // opencode's own Go/Zen credential: launch plain on its own config
		return providers.Target{}, "", true, ErrOwnLogin
	}
	if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
		return gt, gnote, true, nil
	}
	return providers.Target{}, "", false, nil
}

func (e opencodeEngine) noRoute(reg *providers.Registry, _ providers.Target) error {
	return routeErr(e.describeRoute(reg))
}

// launchEnv points every opencode launch at the tracked overlay; a failed write is best-effort, opencode runs without it.
func (opencodeEngine) launchEnv() []string {
	if cfg, tui, err := WriteOpencodeOverlay(); err == nil {
		return []string{"OPENCODE_CONFIG=" + cfg, "OPENCODE_TUI_CONFIG=" + tui}
	}
	return nil
}

func (opencodeEngine) presence(c Cell) error {
	if id, _, found := strings.Cut(c.Model, "/"); found {
		_, err := opencodeCredential(id)
		return err
	}
	return nil
}

// ownLogin is opencode on its own stored login/config: argv only, no harness route injected.
// Tools off still rides OPENCODE_CONFIG_CONTENT, which opencode merges over its config.
func (e opencodeEngine) ownLogin(h Headless) (env, argv []string, err error) {
	if err := checkPrompt(h); err != nil {
		return nil, nil, err
	}
	env, argv = opencodeRun("", h)
	if cred, err := opencodeCredential("opencode"); err == nil {
		env = append(env, cred...) // the vault key when opencode stores no login of its own
	}
	launchEnv, argv := schemaFor(e.name).applyLaunch(nil, argv, false) // headless: never Launch.Args
	return routeEnvLast(launchEnv, env), argv, nil
}

func (opencodeEngine) render(_ *providers.Registry, _ providers.Selection, _ string, o RenderOptions, out io.Writer) (bool, error) {
	return renderOpenCodeOverlay(o, out)
}

func (opencodeEngine) routeDetail() (string, bool) {
	reg, sel, selErr, ok := loadSelection()
	if !ok {
		return "", false
	}
	return opencodeRouteDetail(reg, sel, selErr), true
}

// routeErr is ri's reason as an error when ri resolved no route, else nil.
func routeErr(ri RouteInfo) error {
	if !ri.OK {
		return errors.New(ri.Reason)
	}
	return nil
}

// opencodeSystemAgent is the custom agent whose prompt carries --system-prompt for opencode.
const opencodeSystemAgent = "harness-system"

// opencodeSystemPrompt defines a primary agent with prompt in OpencodeConfigEnv (merged into any config
// already there) and selects it with --agent, the one system-prompt channel opencode run has.
func opencodeSystemPrompt(env, argv []string, prompt string) ([]string, []string) {
	env, _ = mutateOpencodeConfigEnv(env, true, func(cfg map[string]any) {
		agents, _ := cfg["agent"].(map[string]any)
		if agents == nil {
			agents = map[string]any{}
		}
		agents[opencodeSystemAgent] = map[string]any{"mode": "primary", "prompt": prompt}
		cfg["agent"] = agents
	})
	if len(argv) > 0 && argv[0] == "run" {
		argv = append([]string{"run", "--agent", opencodeSystemAgent}, argv[1:]...)
	}
	return env, argv
}

// OpencodeAPIKeyEnv is the vault key a launch passes as env when opencode holds no stored `opencode` login.
const OpencodeAPIKeyEnv = "OPENCODE_API_KEY"

// OpencodeCredID maps a model's provider prefix to the opencode credential serving it; Go and Zen share `opencode`.
func OpencodeCredID(prefix string) string {
	if prefix == "opencode-go" {
		return "opencode"
	}
	return prefix
}

// opencodeCredential resolves prefix's credential: opencode's own stored login first, else (for `opencode`) the
// vault key as launch env; an error names what is missing. The key value rides env only, never output.
func opencodeCredential(prefix string) (env []string, err error) {
	id := OpencodeCredID(prefix)
	if slices.Contains(OpencodeLogins(), id) {
		return nil, nil
	}
	if id == "opencode" {
		if v, _, ok := secrets.Get(OpencodeAPIKeyEnv); ok && strings.TrimSpace(v) != "" {
			return []string{OpencodeAPIKeyEnv + "=" + strings.TrimSpace(v)}, nil
		}
		return nil, fmt.Errorf("opencode has no opencode login (opencode auth login) and no %s in the vault", OpencodeAPIKeyEnv)
	}
	return nil, fmt.Errorf("opencode has no %s login (opencode auth login)", id)
}

// OpencodeOwnLoginPresent reports a usable `opencode` credential (Go/Zen plans): a stored login or the vault key.
func OpencodeOwnLoginPresent() bool {
	_, err := opencodeCredential("opencode-go")
	return err == nil
}
