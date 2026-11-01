package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
)

// Seams for tests.
var (
	findClaude = ClaudeBinary
	selfBinary = agent.SelfBinary
)

// wipeKeys are inherited Anthropic knobs that would override the launch's own resolution.
var wipeKeys = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
	"MAX_THINKING_TOKENS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING", "ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_DISABLE_1M_CONTEXT", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_GATEWAY",
	"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CLAUDE_CODE_ANTHROPIC_MODEL", "OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	providers.EnvSessionProfile, "CLAUDE_MODEL",
	"CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
}

// oauthKey survives the wipe only for cli-auth (subscription login) profiles.
const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// Claude resolves a Claude Code launch.
func Claude(ctx context.Context, o agent.LaunchOptions, args []string) (agent.Plan, error) {
	reg, sel, err := agent.SessionSelection(o, true)
	if err != nil {
		return agent.Plan{}, err
	}
	bin, err := findClaude()
	if err != nil {
		return agent.Plan{}, err
	}
	self, err := selfBinary()
	if err != nil {
		return agent.Plan{}, err
	}
	p := sel.Provider
	passthrough := p.Auth.Method() == providers.AuthCLI
	if p.Auth.Login != "" {
		return agent.Plan{}, fmt.Errorf("profile %s is the %s sign-in and cannot back Claude Code; use claude-code %s --profile %s", p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	plan := agent.Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), wipeKeys...)}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
	}
	if configDirEnvIsDefault() {
		plan.Unset = append(plan.Unset, "CLAUDE_CONFIG_DIR")
	}
	setModelAliasEnv(&plan, sel)
	if err := setMainModel(&plan, sel, p, agent.MainAlias(o), o.Model != "" || vault.GetAgentRecord("claude").Model != ""); err != nil {
		return agent.Plan{}, err
	}
	set1MOff(&plan, sel, p)
	plan.Set[providers.EnvSessionProfile] = p.Name
	agent.OfferLogin(ctx, o, args, sessionProviders(sel), &plan)
	helper, err := configureRouting(ctx, o, reg, sel, p, passthrough, self, &plan)
	if err != nil {
		return agent.Plan{}, err
	}
	if err := applyOverlay(&plan, passthrough, args, helper, p.Env); err != nil {
		return agent.Plan{}, err
	}
	plan.Argv = append(plan.Argv, args...)
	plan.Keep = o.KeepEnv
	keepShellEndpoint(&plan, o)
	return plan, nil
}

// keepShellEndpoint lets a shell ANTHROPIC_MODEL (no --model flag) or ANTHROPIC_BASE_URL win over the vault resolution;
// credentials stay scrubbed.
func keepShellEndpoint(plan *agent.Plan, o agent.LaunchOptions) {
	keep := []string{"ANTHROPIC_BASE_URL"}
	if o.Model == "" {
		keep = append(keep, "ANTHROPIC_MODEL")
	}
	if base := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")); base != "" && !sameEndpoint(base, plan.Set["ANTHROPIC_BASE_URL"]) {
		plan.ForeignBase = true
		dropHarnessCredential(plan)
	}
	for _, k := range keep {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			continue
		}
		delete(plan.Set, k)
		plan.Unset = slices.DeleteFunc(plan.Unset, func(u string) bool { return u == k })
		plan.Keep = append(slices.Clone(plan.Keep), k)
	}
}

// sameEndpoint compares two base URLs by scheme, host and path, ignoring case in the host and a trailing slash.
func sameEndpoint(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/")
}

// dropHarnessCredential strips the apiKeyHelper, headers and setup-token so a foreign endpoint never gets them.
func dropHarnessCredential(plan *agent.Plan) {
	if len(plan.Argv) >= 2 && plan.Argv[0] == "--settings" && plan.Argv[1] == plan.OverlayPath {
		plan.Argv = slices.Clone(plan.Argv[2:])
	}
	plan.Overlay, plan.OverlayPath = nil, ""
	for _, k := range []string{"ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_API_KEY_HELPER_TTL_MS", oauthKey} {
		delete(plan.Set, k)
	}
	plan.Warnings = append(plan.Warnings, "shell ANTHROPIC_BASE_URL kept: the profile credential is withheld; pass your own with --keep-env")
}

// configDirEnvIsDefault: an explicit CLAUDE_CONFIG_DIR=~/.claude makes claude write a stray ~/.claude/.claude.json.
func configDirEnvIsDefault() bool {
	return strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" && paths.ConfigDirIsDefault()
}

// setModelAliasEnv stamps ANTHROPIC_DEFAULT_<SLOT>_MODEL for every alias the selection resolved.
func setModelAliasEnv(plan *agent.Plan, sel providers.Selection) {
	for _, alias := range models.AliasNames {
		if m := sel.ClientModel(alias); m != "" {
			plan.Set["ANTHROPIC_DEFAULT_"+strings.ToUpper(alias)+"_MODEL"] = m
		}
	}
}

// set1MOff turns 1M off when the session endpoint lacks it: Claude Code assumes 1M for recognised ids. An explicit
// [1m] typed on an alias keeps it on. DISABLE_1M_CONTEXT over AUTO_COMPACT_WINDOW: it also drops the 1M /model entries.
func set1MOff(plan *agent.Plan, sel providers.Selection, p *providers.Provider) {
	if p.Serves1M() {
		return
	}
	for _, t := range sel.Aliases {
		if t.OneM {
			return
		}
	}
	plan.Set["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
}

// setMainModel sets the thinking knobs from alias's model, and ANTHROPIC_MODEL only when the user asked for one.
func setMainModel(plan *agent.Plan, sel providers.Selection, p *providers.Provider, alias string, asked bool) error {
	opus := sel.ClientModel(alias)
	if opus == "" {
		return fmt.Errorf("profile %s has no %s model; pin one with claude-code models --pin %s=<id>", p.Name, alias, alias)
	}
	if asked {
		plan.Set["ANTHROPIC_MODEL"] = opus
	}
	if maxTok, disable := models.ThinkingKnobs(opus); maxTok != "" {
		plan.Set["MAX_THINKING_TOKENS"] = maxTok
		if disable != "" {
			plan.Set["CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"] = disable
		}
	}
	return nil
}

// sessionProviders is the session provider plus each alias's provider.
func sessionProviders(sel providers.Selection) []*providers.Provider {
	out := []*providers.Provider{sel.Provider}
	for _, t := range sel.Aliases {
		out = append(out, t.Provider)
	}
	return out
}

// configureRouting wires the router base URL/headers when needed, else the provider's own route, and
// returns the apiKeyHelper command for the overlay.
func configureRouting(ctx context.Context, o agent.LaunchOptions, reg *providers.Registry, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *agent.Plan) (string, error) {
	helper := shellQuote(helperBin(self)) + " resolve --profile " + shellQuote(p.Name)
	for _, t := range sel.Aliases {
		if err := providers.ClaudeCodeError(t.Provider, t.Model); err != nil {
			return "", err
		}
	}
	if sel.NeedsRouter(reg) {
		for _, t := range sel.Aliases {
			if t.Provider.Auth.Method() == providers.AuthCLI && !passthrough {
				return "", fmt.Errorf("model on %s: its cli login needs a cli-login session profile — claude-code switch %s, or pick a key profile", t.Provider.Name, t.Provider.Name)
			}
		}
		base, err := agent.EnsureRouter(ctx)
		if err != nil {
			return "", fmt.Errorf("proxy: %w", err)
		}
		plan.Set["ANTHROPIC_BASE_URL"] = proxy.SessionBase(base, p.Name)
		hdrs := []string{proxy.AgentHeader + ": claude"} // the ledger's agent column; Claude Code adds the session id itself
		if passthrough {
			// The login stays in Authorization, so the router secret rides in its own header.
			secret, err := proxy.ClientSecret()
			if err != nil {
				return "", fmt.Errorf("proxy secret: %w", err)
			}
			hdrs = append(hdrs, proxy.ClientHeader+": "+secret)
		}
		plan.Set["ANTHROPIC_CUSTOM_HEADERS"] = strings.Join(hdrs, "\n")
		plan.OverlayPath = filepath.Join(paths.StateDir(), "overlay-"+p.Name+"-proxy.json")
		return shellQuote(helperBin(self)) + " resolve --proxy", nil
	}
	if route, ok := p.Route(providers.ProtocolAnthropic); ok && route != "https://api.anthropic.com" {
		plan.Set["ANTHROPIC_BASE_URL"] = route
	}
	if h := customHeaders(p.Headers); h != "" {
		plan.Set["ANTHROPIC_CUSTOM_HEADERS"] = h
	}
	if !providers.CredentialPresent(p) {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: no credential found yet (%s); the apiKeyHelper will fail until one is set", p.Name, p.Auth.Env))
	}
	plan.OverlayPath = filepath.Join(paths.StateDir(), "overlay-"+p.Name+".json")
	return helper, nil
}

// applyOverlay sets the passthrough OAuth/unset handling, or the apiKeyHelper overlay + --settings argv.
func applyOverlay(plan *agent.Plan, passthrough bool, args []string, helper string, env map[string]string) error {
	// Provider env (non-secret by Validate) rides the child's process env: Claude Code reads it before settings load.
	for k, v := range providerEnv(env) {
		if _, set := plan.Set[k]; !set { // the harness-resolved key wins
			plan.Set[k] = v
		}
	}
	if passthrough {
		plan.OverlayPath = "" // Claude Code disables subscription OAuth whenever an apiKeyHelper is set
		if !agent.IsHeadless(args) {
			// Remote Control refuses the inference-only setup-token; the interactive /login credential must win.
			plan.Unset = append(plan.Unset, oauthKey)
		} else if os.Getenv(oauthKey) == "" {
			// A long-lived setup-token in secrets.env outlives the /login session in .credentials.json.
			if tok := providers.HeadlessOAuthToken(); tok != "" {
				plan.Set[oauthKey] = tok
			}
		}
		return nil
	}
	plan.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = "300000"
	overlay, err := json.MarshalIndent(map[string]string{"apiKeyHelper": helper}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings overlay: %w", err)
	}
	plan.Overlay = overlay
	plan.Argv = []string{"--settings", plan.OverlayPath}
	return nil
}

// providerEnv is the provider's non-secret Env minus keys the process env or the vault custom env already set.
func providerEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	custom := agent.CustomEnv()
	out := map[string]string{}
	for k, v := range env {
		if _, ok := custom[k]; ok || os.Getenv(k) != "" {
			continue
		}
		out[k] = v
	}
	return out
}

func customHeaders(h map[string]string) string {
	lines := make([]string, 0, len(h))
	for _, k := range slices.Sorted(maps.Keys(h)) {
		lines = append(lines, k+": "+h[k])
	}
	return strings.Join(lines, "\n")
}

// shellQuote single-quotes s when it holds shell metacharacters; apiKeyHelper runs under a shell.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
