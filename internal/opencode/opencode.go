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

	"github.com/edotau/claude-code/internal/agent/launchcli"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	render "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// opencode: a generated provider block via OPENCODE_CONFIG_CONTENT; the key is an {env:} reference.
// Interactive runs also list every alias model and import the caller's Claude permissions + MCP servers.
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
		env = append(env, launchcli.OpenCodeKeyEnv+"="+cred)
	}
	native := dialect == providers.ProtocolAnthropic || (dialect == providers.ProtocolGemini && agent.IsGateway(base))
	return npm, providerOptions(p, cred, url, native), env
}

func opencodeDisableToolStreaming(menu map[string]any) {
	for id, m := range menu { // the router cannot relay fine-grained tool streaming
		if entry, ok := m.(map[string]any); ok {
			entry["options"] = map[string]any{"toolStreaming": false}
			menu[id] = entry
		}
	}
}

// opencodeDatabricksLaunch is the databricks split's env, plus the harness config and (headless) the run argv.
func opencodeDatabricksLaunch(p *providers.Provider, id, dialect, base, model, cred string, routed bool, env []string, h *agent.Headless) ([]string, []string, error) {
	denv, _, err := opencodeDatabricks(p, id, dialect, base, model, cred, routed, env)
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

// interactivePlugin writes the guard plugin for an interactive launch (h nil); a headless run's Exec adds it instead.
// A write failure is an error: the launch must not run without the fail-closed guards.
func interactivePlugin(h *agent.Headless) (string, error) {
	if h != nil {
		return "", nil
	}
	return writePlugin()
}

// opencodeHarnessWith is the per-launch config: the portable core as instructions, the projected commands, tools-off
// when a headless run asks, and the guard plugin url when one was written.
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

// opencodeHarnessConfig is opencodeHarnessWith with the plugin write warned, not returned; launches use the error path.
func opencodeHarnessConfig(h *agent.Headless) func(map[string]any) {
	url, err := interactivePlugin(h)
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "opencode:", err)
	}
	return opencodeHarnessWith(h, url)
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
	return append(argv, launchcli.Positional(h.Prompt)...)
}

// opencodeTitle is the session title: "opencode: " plus the prompt's first 40 runes on one line.
func opencodeTitle(prompt string) string {
	line := strings.Join(strings.Fields(prompt), " ")
	if r := []rune(line); len(r) > 40 {
		line = strings.TrimSpace(string(r[:40]))
	}
	return strings.TrimSpace("opencode: " + line)
}

// opencodeCopilotTarget is opencode's own GitHub Copilot login (`opencode auth login` -> a "github-copilot"
// connection in its stored logins): opus alias, pin-aware. ok is false without that login or a matching registry provider.
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

// OpencodeCopilotLoginPresent reports whether opencode's stored logins (OpencodeAuth: 1.x auth.json or the
// 2.x CLI) include "github-copilot", the integration `opencode auth login` stores for a GitHub Copilot sign-in.
func OpencodeCopilotLoginPresent() bool {
	return slices.Contains(OpencodeLogins(), "github-copilot")
}

// opencodeCopilotLogin runs opencode on its own Copilot sign-in: interactive is plain (ErrOwnLogin), headless pins -m.
func opencodeCopilotLogin(model string, h *agent.Headless) ([]string, []string, error) {
	if !OpencodeCopilotLoginPresent() {
		return nil, nil, errors.New("opencode: no GitHub Copilot login in opencode's stored logins; run `opencode auth login` and choose GitHub Copilot")
	}
	if h == nil {
		return nil, nil, agent.ErrOwnLogin
	}
	env, argv := opencodeRun("github-copilot/"+model, *h)
	return env, argv, nil
}

// OpencodeCopilotModelIDs resolves opencode's github-copilot route's sonnet ("model") and haiku
// ("small_model") wire ids: sonnet carries the everyday coding load, haiku is the cheap alias for
// sub-tasks; each honors CLAUDE_<SLOT>_MODEL then CLAUDE_MODEL the way providers.Select resolves a
// session alias, rooted at the github-copilot provider instead of the session's. ok is false without a
// registered github-copilot provider or either alias.
func OpencodeCopilotModelIDs(reg *providers.Registry) (main, small string, ok bool) {
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
	id := p.WireModel(providers.ParseTarget(reg, spec, p).Model) // a cross-provider pin is respelled for p, not copied
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

// mutateOpencodeConfigEnv applies fn to the JSON in each opencodeConfigEnv entry of env (appending one when absent).
// An unreadable block is an error, or with lenient replaced by an empty config.
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

// opencodeOverlayModels is the overlay's model/small_model: github-copilot/ ids on the copilot-only dispatch (no
// opencode-go login, so the table's opencode-go cells would be unservable), else the table's opencode cells.
func opencodeOverlayModels() (mainModel, smallModel string, ok bool) {
	if !OpencodeOwnLoginPresent() && OpencodeCopilotLoginPresent() {
		if reg, err := providers.Load(); err == nil && reg != nil {
			if m, s, found := OpencodeCopilotModelIDs(reg); found {
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

// RenderSettings is the base launch env, then the effort as --variant on `opencode run` (the TUI has no such flag)
// and the permission policy as config plus OPENCODE_PERMISSION, with --auto.
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
	env, plugged, err := opencodeApplyPlugin(env, req)
	if err != nil {
		return nil, nil, nil, retry.Fatal(name, err)
	}
	host := newHookHost(req)
	env = withHostedMarker(env, host)
	if plugged {
		host.dropPlugin() // the plugin blocks for real; the host's detect-only copies would double-run them
	}
	argv, err = host.before(argv, prompt)
	if err != nil {
		return nil, nil, nil, retry.Fatal(name, err)
	}
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
	if model == "auto" && p.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
		model = p.OpencodeFallback
	}
	return opencodeCopilotLogin(model, h)
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

// PreRoute prefers opencode's own login (Go/Zen, else the github-copilot one) ahead of a session provider that could serve it.
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

// OwnLogin is opencode on its own stored login/config: argv only, no harness route injected.
// Tools off still rides OPENCODE_CONFIG_CONTENT, which opencode merges over its config.
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

func (opencodeEngine) Render(_ *providers.Registry, _ providers.Selection, _ string, o render.RenderOptions, out io.Writer) (bool, error) {
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

// opencodeSystemPrompt defines a primary agent with prompt in opencodeConfigEnv (merged into any config
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
	logins, lerr := opencodeLoginsErr()
	if slices.Contains(logins, id) {
		return nil, nil
	}
	if key := opencodeVaultKey(id); key != "" {
		return []string{OpencodeAPIKeyEnv + "=" + key}, nil
	}
	if lerr != nil { // a failed probe is not "no login": say so rather than sending the user to re-login
		return nil, fmt.Errorf("opencode logins could not be read: %w", lerr)
	}
	if id == "opencode" {
		return nil, missingCredError(fmt.Sprintf("opencode has no opencode login (opencode auth login) and no %s in the vault", OpencodeAPIKeyEnv))
	}
	return nil, missingCredError(fmt.Sprintf("opencode has no %s login (opencode auth login)", id))
}

// missingCredError is opencodeCredential's plain no-login, no-key answer, as opposed to a failed read.
type missingCredError string

func (e missingCredError) Error() string { return string(e) }

// opencodeVaultKey is the vault's opencode API key for the `opencode` credential id, else "".
func opencodeVaultKey(id string) string {
	if id != "opencode" {
		return ""
	}
	v, _, _ := vault.Get(OpencodeAPIKeyEnv)
	return strings.TrimSpace(v)
}

// ownLoginTTL bounds how long OpencodeOwnLoginPresent reuses an answer: one launch's route calls, not a later vault edit.
const ownLoginTTL = 2 * time.Second

// ownLoginKey is the auth state and key env an OpencodeOwnLoginPresent answer was computed under.
type ownLoginKey struct{ key, env string }

var ownLogin paths.Memo[ownLoginKey, bool] // InvalidateOpencodeAuth drops it; a probe in flight then is not kept

// OpencodeOwnLoginPresent reports a usable `opencode` credential (Go/Zen plans): a stored login or the vault key.
// The answer is reused for ownLoginTTL, so a launch's route calls read the vault once.
func OpencodeOwnLoginPresent() bool {
	_, _, key := opencodeAuthKeys()
	ok, _ := ownLogin.Do(ownLoginKey{key, os.Getenv(OpencodeAPIKeyEnv)}, func(bool, error) time.Duration { return ownLoginTTL },
		func() (bool, error) {
			_, err := opencodeCredential("opencode-go")
			return err == nil, nil
		})
	return ok
}
