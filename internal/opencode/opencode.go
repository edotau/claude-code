package opencode

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// opencode generates provider config and projects rules, commands, plugin and menu for interactive runs.
func opencode(p *providers.Provider, dialect, base, model, cred string, routed bool, h *agent.Headless) ([]string, []string, error) {
	id, err := agentconfig.OwnedID(p)
	if err != nil {
		return nil, nil, err
	}
	npm, options, env := opencodeProviderOptions(p, dialect, base, cred)
	if p.Preset == providers.PresetDatabricks {
		return opencodeDatabricksLaunch(p, id, dialect, base, model, cred, routed, env, h)
	}
	menu := opencodeModels(p, model, h)
	if routed && dialect == providers.ProtocolAnthropic {
		opencodeDisableToolStreaming(menu)
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   id + "/" + model,
		"provider": map[string]any{id: map[string]any{
			"npm": npm, "name": "claude-code", "options": options,
			"models": menu,
		}},
	}
	url, err := interactivePlugin(h)
	if err != nil {
		return nil, nil, err
	}
	opencodeHarnessWith(h, url)(cfg)
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	env = append(modelsDefined(env), opencodeConfigEnv+"="+string(body))
	if h == nil {
		return env, nil, nil
	}
	if !h.Tools {
		env = append(env, opencodePermissionEnv+`="deny"`)
	}
	return env, opencodeArgv(id+"/"+model, *h), nil
}

func opencodeProviderOptions(p *providers.Provider, dialect, base, cred string) (string, map[string]any, []string) {
	npm, url := "@ai-sdk/openai-compatible", base
	// Native SDKs keep provider-specific state (Gemini 3 thought signatures) the openai-compatible shim drops.
	switch dialect {
	case providers.ProtocolAnthropic:
		npm, url = "@ai-sdk/anthropic", base+"/v1"
	case providers.ProtocolGemini:
		npm, url = "@ai-sdk/google", base+"/v1beta"
	}
	var env []string
	if cred != "" {
		env = append(env, openCodeKeyEnv+"="+cred)
	}
	native := dialect == providers.ProtocolAnthropic || (dialect == providers.ProtocolGemini && agent.IsGateway(base))
	return npm, providerOptions(p, cred, url, native), env
}

func opencodeDisableToolStreaming(menu map[string]any) {
	for id, m := range menu { // the router cannot relay fine-grained tool streaming
		if entry, ok := m.(map[string]any); ok {
			menu[id] = claudeEntry(entry)
		}
	}
}

// opencodeDatabricksLaunch is the databricks split's env, plus the harness config and (headless) the run argv.
func opencodeDatabricksLaunch(p *providers.Provider, id, dialect, base, model, cred string, routed bool, env []string, h *agent.Headless) ([]string, []string, error) {
	denv, err := opencodeDatabricks(p, id, dialect, base, model, cred, routed, env)
	if err != nil {
		return denv, nil, err
	}
	url, err := interactivePlugin(h)
	if err != nil {
		return nil, nil, err
	}
	if denv, err = mutateOpencodeConfigEnv(denv, false, opencodeHarnessWith(h, url)); err != nil {
		return nil, nil, err
	}
	if h == nil {
		return denv, nil, nil
	}
	if !h.Tools {
		denv = append(denv, opencodePermissionEnv+`="deny"`) // an inherited value would override the tools-off deny
	}
	return denv, opencodeArgv(opencodeDatabricksModel(denv), *h), nil
}

// interactivePlugin writes interactive guards; failures abort launch to keep guards fail-closed.
func interactivePlugin(h *agent.Headless) (string, error) {
	if h != nil {
		return "", nil
	}
	return writePlugin()
}

// opencodeHarnessWith builds per-launch instructions, commands, tools-off policy and the guard plugin URL.
func opencodeHarnessWith(h *agent.Headless, url string) func(map[string]any) {
	return func(cfg map[string]any) {
		opencodeRulesOn(cfg)
		opencodeCommandsOn(cfg)
		if h != nil && !h.Tools {
			opencodeToolsOff(cfg)
		}
		if url != "" {
			addPlugin(cfg, url)
		}
	}
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

// opencodeModels is the model menu: the requested model, plus every alias for an interactive launch.
func opencodeModels(p *providers.Provider, model string, h *agent.Headless) map[string]any {
	menu := map[string]any{model: map[string]any{"name": model}}
	if h == nil {
		for _, alias := range models.AliasNames {
			if id := models.Strip1M(p.Models[alias]); id != "" {
				menu[id] = map[string]any{"name": id}
			}
		}
		if p.Provider == "github" {
			return menu // Saved history must not re-enable a disabled Copilot model.
		}
		saved, err := providers.PickerModels(p)
		if err != nil {
			fmt.Fprintln(agentconfig.CatalogWarn, "opencode:", err)
		}
		for _, m := range saved {
			if _, exists := menu[m.ID]; !exists {
				menu[m.ID] = map[string]any{"name": cmp.Or(m.Name, m.ID) + " · " + p.Name}
			}
		}
	}
	return menu
}

// opencodeToolsOff is the tools-off config: every tool denied and every MCP server the overlay configures disabled.
func opencodeToolsOff(cfg map[string]any) {
	cfg["permission"] = "deny" // every tool, incl. task subagents and websearch — parity with claude --tools ""
	if mcp := opencodeMCPOff(); len(mcp) > 0 {
		cfg["mcp"] = mcp // opencode starts every MCP server even when no tool can run (~9 s for chrome-devtools)
	}
}

// opencodeMCPOff disables each MCP server named in the rendered overlay; nil when it cannot be rendered.
func opencodeMCPOff() map[string]any {
	_, body, _, _, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		return nil
	}
	var doc struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	off := make(map[string]any, len(doc.MCP))
	for name := range doc.MCP {
		off[name] = map[string]any{"enabled": false}
	}
	return off
}

// opencodeRun is `opencode run --format json` for h; a non-empty model adds -m (opencode's provider/model form).
func opencodeRun(model string, h agent.Headless) (env, argv []string) {
	if !h.Tools {
		env, _ = mutateOpencodeConfigEnv(nil, true, opencodeToolsOff)
		env = append(env, opencodePermissionEnv+`="deny"`) // pin it: an inherited value would override the tools-off deny
	}
	return env, opencodeArgv(model, h)
}

// opencodeArgv is the `opencode run` argv for h; model "" leaves -m to opencode's own config.
func opencodeArgv(model string, h agent.Headless) []string {
	argv := []string{"run", "--format", "json"}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	if h.WorkDir != "" {
		argv = append(argv, "--dir", h.WorkDir)
	}
	if h.SessionID != "" {
		argv = append(argv, "--session", h.SessionID)
	} else {
		argv = append(argv, "--title", opencodeTitle(h.Prompt)) // an explicit title skips opencode's extra title-model call
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
		return append(append(argv, "--"), h.Prompt)
	}
	return append(argv, positional(h.Prompt)...)
}

// opencodeTitle is the session title: "opencode: " plus the prompt's first 40 runes on one line.
func opencodeTitle(prompt string) string {
	line := textutil.TruncateRunes(strings.Join(strings.Fields(prompt), " "), 40)
	return strings.TrimSpace("opencode: " + line)
}

// opencodeCopilotTarget resolves the pin-aware opus route on opencode's own github-copilot login, if registered.
func opencodeCopilotTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if !OpencodeCopilotLoginPresent() {
		return providers.Target{}, "", false
	}
	p, ok := reg.LoginProfile("github", "copilot")
	if !ok {
		return providers.Target{}, "", false
	}
	return providers.Target{Provider: p, Model: p.Models[models.Opus]}, "opencode's own GitHub Copilot login (opencode auth login)", true
}

// OpencodeCopilotLoginPresent checks opencode's stored github-copilot login (1.x auth.json or 2.x CLI).
func OpencodeCopilotLoginPresent() bool {
	return slices.Contains(listOpencodeLogins(), "github-copilot")
}

// OpencodeCopilotToken is the GitHub token behind opencode 1.x's own Copilot sign-in (auth.json), "" when it has none.
func OpencodeCopilotToken() string {
	data, err := os.ReadFile(opencodeAuthFile())
	if err != nil {
		return ""
	}
	var m map[string]struct {
		Refresh string `json:"refresh"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for k, v := range m {
		if strings.TrimSuffix(k, "/") == "github-copilot" && v.Refresh != "" {
			return v.Refresh
		}
	}
	return ""
}

// opencodeCopilotModelIDs resolves pin-aware sonnet/haiku IDs under github-copilot; false if either alias is missing.
func opencodeCopilotModelIDs(reg *providers.Registry) (main, small string, ok bool) {
	p, found := reg.LoginProfile("github", "copilot")
	if !found {
		return "", "", false
	}
	main = aliasModelID(reg, p, models.Sonnet)
	small = aliasModelID(reg, p, models.Haiku)
	return main, small, main != "" && small != ""
}

// aliasModelID is alias's pin-resolved wire model id for p: CLAUDE_<SLOT>_MODEL, else CLAUDE_MODEL, else
// p's own table. A pin naming a different provider ("other:model") is respelled the way p serves it. A
// resolved id of "auto" (a router alias opencode's own provider catalog does not list) is swapped for
// p.OpencodeFallback when the registry sets one — config-driven, no model id hardcoded here.
func aliasModelID(reg *providers.Registry, p *providers.Provider, alias string) string {
	rec := vault.GetAgentRecord("claude")
	spec := cmp.Or(rec.Aliases[strings.ToLower(alias)], rec.Model)
	if spec == "" {
		spec = p.Models[alias]
	}
	if spec == "" {
		return ""
	}
	return opencodeModel(p, p.WireModel(providers.ParseTarget(reg, spec, p).Model)) // a cross-provider pin is respelled for p
}

// opencodeModel swaps Copilot's "auto" router id (absent from opencode's catalog) for p.OpencodeFallback.
func opencodeModel(p *providers.Provider, id string) string {
	if id == "auto" && p.OpencodeFallback != "" {
		return p.OpencodeFallback
	}
	return id
}

// OwnLogin is opencode's own-login profile: the free Zen catalog then the paid ladder, as the old unset pin.
const OwnLogin = "zen-free"

// opencodeConfigEnv is the env var carrying opencode's inline JSON config.
const opencodeConfigEnv = "OPENCODE_CONFIG_CONTENT"

const opencodePermissionEnv = "OPENCODE_PERMISSION"

// opencodeNoModelsFetch skips opencode's startup models.dev fetch; set only when the inline config defines the models.
const opencodeNoModelsFetch = "OPENCODE_DISABLE_MODELS_FETCH"

// modelsDefined adds opencodeNoModelsFetch=1 unless the user's shell already sets it (their value wins).
func modelsDefined(env []string) []string {
	if _, set := os.LookupEnv(opencodeNoModelsFetch); set {
		return env
	}
	return append(env, opencodeNoModelsFetch+"=1")
}

// mutateOpencodeConfigEnv edits each inline config (appending if absent); lenient replaces unreadable blocks.
func mutateOpencodeConfigEnv(env []string, lenient bool, fn func(map[string]any)) ([]string, error) {
	found := false
	env = slices.Clone(env)
	for i, kv := range env {
		body, ok := strings.CutPrefix(kv, opencodeConfigEnv+"=")
		if !ok {
			continue
		}
		found = true
		out, err := mutateConfigJSON(body, lenient, fn)
		if err != nil {
			return nil, err
		}
		env[i] = opencodeConfigEnv + "=" + out
	}
	if found {
		return env, nil
	}
	out, err := mutateConfigJSON("{}", false, fn)
	if err != nil {
		return nil, err
	}
	return append(env, opencodeConfigEnv+"="+out), nil
}

// mutateConfigJSON applies fn to the JSON object in body; unreadable JSON is an error unless lenient (then empty).
func mutateConfigJSON(body string, lenient bool, fn func(map[string]any)) (string, error) {
	var cfg map[string]any
	if err := json.Unmarshal([]byte(body), &cfg); err != nil && !lenient {
		return "", err
	}
	if cfg == nil { // unreadable (lenient) or JSON null
		cfg = map[string]any{}
	}
	fn(cfg)
	out, err := json.Marshal(cfg)
	return string(out), err
}

// opencodeOverlayModels selects github-copilot/ cells for copilot-only dispatches, else the table's opencode cells.
func opencodeOverlayModels() (mainModel, smallModel string, ok bool) {
	if !OpencodeOwnLoginPresent() && OpencodeCopilotLoginPresent() {
		if reg, err := providers.Load(); err == nil && reg != nil {
			if m, s, found := opencodeCopilotModelIDs(reg); found {
				return "github-copilot/" + m, "github-copilot/" + s, true
			}
		}
	}
	return router.OpencodeTableModels()
}

func init() { agentconfig.OverlayModels = opencodeOverlayModels }

type opencodeEngine struct{ agent.BaseEngine }

var _ agent.HeadlessDriver = opencodeEngine{}

func (opencodeEngine) After(p *agent.CLIPrep, req agent.RunOptions) {
	if req.SystemPrompt != "" {
		p.Env, p.Argv = opencodeSystemPrompt(p.Env, p.Argv, req.SystemPrompt)
	}
}

func (e opencodeEngine) Exec(ctx context.Context, p agent.CLIPrep, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if err := agent.CheckCapabilities(e.Name(), req); err != nil {
		return p.Result, err
	}
	if err := refusePolicyEnv(req); err != nil {
		return p.Result, err
	}
	bin, err := e.Bin()
	if err != nil {
		return p.Result, err
	}
	cmd, tail, host, err := opencodeChild(ctx, e.Name(), bin, p.Env, p.Argv, p.Prompt, req)
	if err != nil {
		return p.Result, err
	}
	res, err := agent.RunStream(ctx, e.Name(), cmd, tail, p.Result, sink, host.parser(nil))
	host.after(res)
	if err == nil {
		err = refuseToolCallText(e.Name(), req, res.Answer)
	}
	return res, err
}

// RenderSettings applies launch env, run-only effort --variant, and permission config with --auto.
func (e opencodeEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	env, argv, emulated, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	if s.Effort != "" && len(argv) > 0 && argv[0] == "run" && !slices.Contains(flagsOnly(argv), "--variant") {
		argv = slices.Insert(slices.Clone(argv), 1, "--variant", s.Effort)
	}
	req := agent.RunOptions{Tools: s.Tools, SafeMode: s.SafeMode, Permissions: s.Permissions}
	if env, err = opencodeApplyPermissions(env, req); err != nil {
		return nil, nil, emulated, cleanup, err
	}
	return env, opencodePolicyArgv(argv, req), emulated, cleanup, nil
}

// opencodeChild loads the plugin and hook host over env/argv and builds the child; its errors are Fatal for name.
func opencodeChild(ctx context.Context, name, bin string, env, argv []string, prompt string, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer, *hookHost, error) {
	if err := agent.CheckCapabilities(name, req); err != nil {
		return nil, nil, nil, err
	}
	var err error
	if env, err = opencodeApplyPermissions(env, req); err != nil {
		return nil, nil, nil, retry.Fatal(name, err)
	}
	env, plugged, err := opencodeApplyPlugin(env, req)
	if err != nil {
		return nil, nil, nil, retry.Fatal(name, err)
	}
	host := newHookHost(req)
	env = withHostedMarker(env, host)
	if plugged {
		host.dropPlugin() // the plugin blocks for real; the host's copies would double-run them
	}
	argv, err = host.before(argv, prompt)
	if err != nil {
		return nil, nil, nil, retry.Fatal(name, err)
	}
	ctx = agent.WithPermissionsEnv(ctx, GatePolicy(req))
	cmd, tail := agent.ChildCmd(ctx, bin, argv, env, req.WorkDir, "", req.KeepEnv, true)
	return cmd, tail, host, nil
}

// LoginServes adds github: that profile's Auth.Login names copilot, yet opencode has its own GitHub Copilot login.
func (e opencodeEngine) LoginServes(p *providers.Provider) bool {
	return p.Provider == "github" || e.BaseEngine.LoginServes(p)
}

func (opencodeEngine) Login(p *providers.Provider, model string, h *agent.Headless) (env, argv []string, err error) {
	if p.Provider != "github" {
		return nil, nil, errors.New("opencode: no subscription sign-in support")
	}
	if !OpencodeCopilotLoginPresent() {
		return nil, nil, errors.New("opencode: no GitHub Copilot login in opencode's stored logins; run `opencode auth login` and choose GitHub Copilot")
	}
	if h == nil { // interactive runs plain on its own login; headless pins -m
		return nil, nil, agent.ErrOwnLogin
	}
	env, argv = opencodeRun("github-copilot/"+opencodeModel(p, model), *h)
	return env, argv, nil
}

func (opencodeEngine) Wire(ctx context.Context, r agent.WireRoute, h *agent.Headless) (env, argv []string, err error) {
	return opencode(r.Provider, r.Dialect, r.Base, r.Model, r.Cred, r.Routed, h)
}

func (e opencodeEngine) FallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return agent.RouteFallback(reg, e.DescribeRoute(reg))
}

func (opencodeEngine) DescribeRoute(reg *providers.Registry) agent.RouteInfo {
	return describeOpenCodeRoute(reg)
}

var _ agent.Authenticator = opencodeEngine{}

// AuthStatus is opencode's stored `opencode auth login` providers; a failed probe sets Err, never reads as logged out.
func (opencodeEngine) AuthStatus() providers.AuthState {
	names, err := opencodeLoginsErr()
	if len(names) == 0 {
		return providers.AuthState{Err: err}
	}
	return providers.AuthState{
		Present: true, Source: "opencode auth login: " + strings.Join(names, ", "), Method: providers.AuthCLI, Err: err,
	}
}

func (opencodeEngine) Plan(context.Context, providers.AuthState) providers.Subscription {
	return providers.Subscription{}
}

// Verify is presence-only: a probe error with no stored login is transient, not NeedsLogin.
func (e opencodeEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	st := e.AuthStatus()
	switch {
	case st.Present:
		return providers.VerdictOK, st.Source
	case st.Err != nil:
		return providers.VerdictTransient, st.Err.Error()
	}
	return providers.VerdictNeedsLogin, ""
}

// PreRoute prefers opencode's own login (Go/Zen, else github-copilot) ahead of a session provider that could serve it.
func (opencodeEngine) PreRoute(reg *providers.Registry, explicit bool) (providers.Target, string, bool, error) {
	if explicit {
		return providers.Target{}, "", false, nil
	}
	if OpencodeOwnLoginPresent() { // opencode's own Go/Zen credential: launch plain on its own config
		return providers.Target{}, "", true, agent.ErrOwnLogin
	}
	if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
		return gt, gnote, true, nil
	}
	return providers.Target{}, "", false, nil
}

func (e opencodeEngine) NoRoute(reg *providers.Registry, _ providers.Target) error {
	return agent.RouteErr(e.DescribeRoute(reg))
}

// LaunchEnv points every opencode launch at the tracked overlay; each error is a notice and opencode runs without it.
func (opencodeEngine) LaunchEnv() []string {
	env, err := opencodeCredential("opencode")
	var missing missingCredError
	if err != nil && !errors.As(err, &missing) { // no login and no key is the Databricks-pinned norm, not news
		fmt.Fprintln(agent.NoticeOut, "opencode:", err)
	}
	cfg, tui, err := agentconfig.WriteOpencodeOverlay()
	if err != nil {
		fmt.Fprintln(agent.NoticeOut, "opencode: overlay:", err)
		return env
	}
	return append(env, "OPENCODE_CONFIG="+cfg, "OPENCODE_TUI_CONFIG="+tui)
}

func (opencodeEngine) Presence(c router.Cell) error {
	if id, _, found := strings.Cut(c.Model, "/"); found {
		_, err := opencodeCredential(id)
		return err
	}
	return nil
}

// OwnLogin uses stored login/config with argv only, adding inline config when tools are off.
func (opencodeEngine) OwnLogin(h agent.Headless) (env, argv []string, err error) {
	if err := agent.CheckPrompt(h); err != nil {
		return nil, nil, err
	}
	env, argv = opencodeRun("", h)
	if cred, err := opencodeCredential("opencode"); err == nil {
		env = append(env, cred...) // the vault key when opencode stores no login of its own
	}
	return env, argv, nil // prepare's RenderSettings adds the launch env
}

func (opencodeEngine) Render(_ *providers.Registry, _ providers.Selection, _ string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	return renderOpenCodeOverlay(o, out)
}

func (opencodeEngine) RouteDetail() (string, bool) {
	reg, sel, selErr, ok := agent.LoadSelection()
	if !ok {
		return "", false
	}
	return opencodeRouteDetail(reg, sel, selErr), true
}

// opencodeSystemAgent is the custom agent whose prompt carries --system-prompt for opencode.
const opencodeSystemAgent = "harness-system"

// opencodeSystemPrompt merges a primary agent carrying --system-prompt into inline config and selects it with --agent.
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

// opencodeAPIKeyEnv is the vault key a launch passes as env when opencode holds no stored `opencode` login.
const opencodeAPIKeyEnv = "OPENCODE_API_KEY"

// openCodeKeyEnv carries the credential (never argv); the config text names the variable, not the secret.
const openCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"

// positional guards a dash-led prompt from flag parsing.
func positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}

// OpencodeCredID maps a model's provider prefix to the opencode credential serving it; Go and Zen share `opencode`.
func OpencodeCredID(prefix string) string {
	if prefix == "opencode-go" {
		return "opencode"
	}
	return prefix
}

// opencodeCredential uses stored login then the opencode vault key via env only; missing credentials are errors.
func opencodeCredential(prefix string) (env []string, err error) {
	id := OpencodeCredID(prefix)
	logins, lerr := opencodeLoginsErr()
	if slices.Contains(logins, id) {
		return nil, nil
	}
	key, verr := opencodeVaultKey(id)
	if key != "" {
		return []string{opencodeAPIKeyEnv + "=" + key}, nil
	}
	if verr != nil { // a locked or corrupt vault is not "no key"
		return nil, fmt.Errorf("opencode: %s could not be read from the vault: %w", opencodeAPIKeyEnv, verr)
	}
	if lerr != nil { // a failed probe is not "no login": say so rather than sending the user to re-login
		return nil, fmt.Errorf("opencode logins could not be read: %w", lerr)
	}
	if id == "opencode" {
		return nil, missingCredError(fmt.Sprintf("opencode has no opencode login (opencode auth login) and no %s in the vault", opencodeAPIKeyEnv))
	}
	return nil, missingCredError(fmt.Sprintf("opencode has no %s login (opencode auth login)", id))
}

// missingCredError is opencodeCredential's plain no-login, no-key answer, as opposed to a failed read.
type missingCredError string

func (e missingCredError) Error() string { return string(e) }

// opencodeVaultKey is the vault's opencode API key for the `opencode` credential id, else ""; err is a failed read.
func opencodeVaultKey(id string) (string, error) {
	if id != "opencode" {
		return "", nil
	}
	v, _, err := vault.Lookup(opencodeAPIKeyEnv)
	return strings.TrimSpace(v), err
}

// ownLoginTTL bounds how long OpencodeOwnLoginPresent reuses an answer: one launch's route calls, not a later edit
const ownLoginTTL = 2 * time.Second

// ownLoginKeep keeps an answer for ownLoginTTL and a failed probe not at all.
func ownLoginKeep(_ bool, err error) time.Duration {
	if err != nil {
		return -1
	}
	return ownLoginTTL
}

// ownLoginKey is the auth state and key env an OpencodeOwnLoginPresent answer was computed under.
type ownLoginKey struct{ key, env string }

var ownLogin paths.Memo[ownLoginKey, bool] // InvalidateOpencodeAuth drops it; a probe in flight then is not kept

// OpencodeOwnLoginPresent checks stored Go/Zen login or vault key, reusing the answer for ownLoginTTL.
func OpencodeOwnLoginPresent() bool {
	_, _, key := opencodeAuthKeys()
	ok, _ := ownLogin.Do(ownLoginKey{key, os.Getenv(opencodeAPIKeyEnv)}, ownLoginKeep,
		func() (bool, error) {
			_, err := opencodeCredential("opencode-go")
			if err != nil && !errors.As(err, new(missingCredError)) {
				return false, err // an unreadable login or vault is not an answer: do not keep it
			}
			return err == nil, nil
		})
	return ok
}
