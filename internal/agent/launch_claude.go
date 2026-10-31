package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// Seams for tests.
var (
	findClaude = ClaudeBinary
	lookPath   = paths.VendorBin
	selfBinary = SelfBinary
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
	providers.EnvSessionProfile, providers.PinModel,
	providers.SlotPin(models.Opus), providers.SlotPin(models.Sonnet), providers.SlotPin(models.Haiku), providers.SlotPin(models.Fable),
}

// oauthKey survives the wipe only for cli-auth (subscription login) profiles.
const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// selection loads the registry and applies the per-launch profile/model flags.
func selection(o LaunchOptions) (*providers.Registry, providers.Selection, error) {
	return sessionSelection(o, false)
}

// sessionSelection loads the registry and applies --profile/--model > pins > the workspace's last profile > defaults;
// session marks a Claude launch.
func sessionSelection(o LaunchOptions, session bool) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	if session && o.Provider == "" && providers.Pin(providers.PinProfile) == "" {
		o.Provider = lastProfile(reg)
	}
	sel, err := providers.Select(reg, o.Provider)
	if o.Model != "" && sel.Provider != nil {
		applyModel(&sel, o.Model, session, o.Tier)
		err = nil
	}
	return reg, sel, err
}

// MainTier is the tier whose model is the launch's main model: the family --model names, else the verb's tier, else opus.
func MainTier(o LaunchOptions) string {
	if t, _, _ := parseModelFlag(o.Model); t != "" {
		return t
	}
	return cmp.Or(o.Tier, models.Opus)
}

// parseModelFlag reads --model: tier is the Claude family it names ("" for a non-Claude wire id), canonical its
// catalog id ("" when unknown), alias is true when the input was a bare tier alias (opus), which floats.
func parseModelFlag(s string) (tier, canonical string, alias bool) {
	s = strings.TrimSpace(s)
	name := strings.ToLower(models.Strip1M(s))
	if c, ok := models.Alias(name); ok && slices.Contains(models.Slots, name) {
		return name, c, !models.Has1M(s)
	}
	canonical, _, ok := models.Resolve(s)
	if !ok {
		return "", "", false
	}
	if f, _, _, ok := models.ParseClaudeID(canonical); ok && slices.Contains(models.Slots, f) {
		return f, canonical, false
	}
	return "", canonical, false
}

// applyModel applies --model to sel: a bare alias keeps the tier's resolution, else the main slot.
func applyModel(sel *providers.Selection, model string, session bool, verbTier string) {
	tier, canonical, alias := parseModelFlag(model)
	dst := models.Opus
	if session {
		dst = MainTier(LaunchOptions{Model: model, Tier: verbTier})
	}
	if alias {
		if t, ok := sel.Slots[tier]; ok && dst != tier {
			sel.Slots[dst] = t
		}
		return
	}
	wire := strings.TrimSpace(model)
	oneM := models.Has1M(wire)
	wire = models.Strip1M(wire)
	if canonical != "" {
		wire = sel.Provider.WireModel(canonical)
	}
	sel.Slots[dst] = providers.Target{Provider: sel.Provider, Model: wire, OneM: oneM}
}

// Claude resolves a Claude Code launch.
func Claude(ctx context.Context, o LaunchOptions, args []string) (Plan, error) {
	reg, sel, err := sessionSelection(o, true)
	if err != nil {
		return Plan{}, err
	}
	bin, err := findClaude()
	if err != nil {
		return Plan{}, err
	}
	self, err := selfBinary()
	if err != nil {
		return Plan{}, err
	}
	p := sel.Provider
	passthrough := p.Auth.Method() == providers.AuthCLI
	if p.Auth.Login != "" {
		return Plan{}, fmt.Errorf("profile %s is the %s sign-in and cannot back Claude Code; use claude-code %s --profile %s", p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	plan := Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), wipeKeys...)}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
	}
	if configDirEnvIsDefault() {
		plan.Unset = append(plan.Unset, "CLAUDE_CONFIG_DIR")
	}
	setModelSlotEnv(&plan, sel)
	if err := setMainModel(&plan, sel, p, MainTier(o), o.Model != "" || providers.Pin(providers.PinModel) != ""); err != nil {
		return Plan{}, err
	}
	set1MOff(&plan, sel, p)
	plan.Set[providers.EnvSessionProfile] = p.Name
	offerLogin(ctx, o, args, sessionProviders(sel), &plan)
	helper, err := configureRouting(ctx, o, reg, sel, p, passthrough, self, &plan)
	if err != nil {
		return Plan{}, err
	}
	if err := applyOverlay(&plan, passthrough, args, helper, p.Env); err != nil {
		return Plan{}, err
	}
	plan.Argv = append(plan.Argv, args...)
	plan.Keep = o.KeepEnv
	return plan, nil
}

// configDirEnvIsDefault: an explicit CLAUDE_CONFIG_DIR=~/.claude makes claude write a stray ~/.claude/.claude.json.
func configDirEnvIsDefault() bool {
	home, err := os.UserHomeDir()
	d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	return err == nil && d != "" && filepath.Clean(d) == filepath.Join(home, ".claude")
}

// setModelSlotEnv stamps ANTHROPIC_DEFAULT_<SLOT>_MODEL for every slot the selection resolved.
func setModelSlotEnv(plan *Plan, sel providers.Selection) {
	for _, slot := range models.Slots {
		if m := sel.ClientModel(slot); m != "" {
			plan.Set["ANTHROPIC_DEFAULT_"+strings.ToUpper(slot)+"_MODEL"] = m
		}
	}
}

// set1MOff turns 1M off when the session endpoint lacks it: Claude Code assumes 1M for recognised ids. An explicit
// [1m] typed on a slot keeps it on. DISABLE_1M_CONTEXT over AUTO_COMPACT_WINDOW: it also drops the 1M /model entries.
func set1MOff(plan *Plan, sel providers.Selection, p *providers.Provider) {
	if p.Serves1M() {
		return
	}
	for _, t := range sel.Slots {
		if t.OneM {
			return
		}
	}
	plan.Set["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
}

// setMainModel sets the thinking knobs from tier's model, and ANTHROPIC_MODEL only when the user asked for one.
func setMainModel(plan *Plan, sel providers.Selection, p *providers.Provider, tier string, asked bool) error {
	opus := sel.ClientModel(tier)
	if opus == "" {
		return fmt.Errorf("profile %s has no %s model; pin one with claude-code models --pin %s=<id>", p.Name, tier, tier)
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

// sessionProviders is the session provider plus each slot's provider.
func sessionProviders(sel providers.Selection) []*providers.Provider {
	out := []*providers.Provider{sel.Provider}
	for _, t := range sel.Slots {
		out = append(out, t.Provider)
	}
	return out
}

// configureRouting wires the router base URL/headers when needed, else the provider's own route, and
// returns the apiKeyHelper command for the overlay.
func configureRouting(ctx context.Context, o LaunchOptions, reg *providers.Registry, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *Plan) (string, error) {
	helper := shellQuote(helperBin(self)) + " resolve --profile " + shellQuote(p.Name)
	for _, t := range sel.Slots {
		if err := providers.ClaudeCodeError(t.Provider, t.Model); err != nil {
			return "", err
		}
	}
	if sel.NeedsRouter(reg) {
		for _, t := range sel.Slots {
			if t.Provider.Auth.Method() == providers.AuthCLI && !passthrough {
				return "", fmt.Errorf("model on %s: its cli login needs a cli-login session profile — claude-code switch %s, or pick a key profile", t.Provider.Name, t.Provider.Name)
			}
		}
		base, err := ensureRouter(ctx)
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
func applyOverlay(plan *Plan, passthrough bool, args []string, helper string, env map[string]string) error {
	// Provider env (non-secret by Validate) rides the child's process env: Claude Code reads it before settings load.
	for k, v := range providerEnv(env) {
		if _, set := plan.Set[k]; !set { // the harness-resolved key wins
			plan.Set[k] = v
		}
	}
	if passthrough {
		plan.OverlayPath = "" // Claude Code disables subscription OAuth whenever an apiKeyHelper is set
		if !headless(args) {
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
	custom := customEnv()
	out := map[string]string{}
	for k, v := range env {
		if _, ok := custom[k]; ok || os.Getenv(k) != "" {
			continue
		}
		out[k] = v
	}
	return out
}

// headless reports a `claude -p`/`--print` run, the only launch that carries the setup-token.
func headless(args []string) bool {
	return slices.Contains(args, "-p") || slices.Contains(args, "--print")
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
