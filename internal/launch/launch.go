// Package launch builds and execs Claude Code and vendor agent CLIs against the provider registry.
package launch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/envscrub"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/removedflags"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/vendors"
	"github.com/edotau/claude-code/internal/vendorsettings"
)

// Options are the harness flags consumed before passthrough.
type Options struct {
	Provider string // the profile name from --profile (field name kept until the agent lane moves off it)
	Model    string // alias, canonical version or wire id, this launch only
	Tier     string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	PrintEnv bool
	Direct   bool     // agents only: skip the proxy, hand the CLI the profile route
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
	File     string   // codex only: a config.toml whose keys ride as -c overrides after the harness's, from -f
}

// flagValue is the value of the flag at args[i]: val when inline (--flag=val), else the next arg and its index.
func flagValue(args []string, i int, name, val string, hasVal bool) (string, int, error) {
	if hasVal {
		return val, i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s needs a value", name)
	}
	return args[i+1], i + 1, nil
}

// ParseArgs strips harness flags from args; the first "--" ends harness parsing and is dropped.
func ParseArgs(args []string, claude bool) (Options, []string, error) {
	var o Options
	var rest []string
	if err := removedflags.Hint(args); err != nil {
		return o, nil, err
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			return o, append(rest, args[i+1:]...), nil
		case name == "--profile" || name == "--model":
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			if name == "--profile" {
				o.Provider = val
			} else {
				o.Model = val
			}
		case a == "--direct" && !claude:
			o.Direct = true
		case a == "--print-env":
			o.PrintEnv = true
		case (name == "-f" || name == "--file") && !claude:
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			o.File = val
		case name == "--keep-env":
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			o.KeepEnv = append(o.KeepEnv, strings.Split(val, ",")...)
		default:
			rest = append(rest, a)
		}
	}
	return o, rest, nil
}

// Plan is a fully resolved launch: exec Binary with Argv under the base env minus Unset plus Set.
type Plan struct {
	Binary      string
	Argv        []string
	Set         map[string]string
	Unset       []string
	Keep        []string // names the credential scrub must not drop, from Options.KeepEnv
	Vendor      bool     // true for a vendor CLI child: HARNESS_VAULT_KEY is scrubbed too (never for Claude itself)
	OverlayPath string
	Overlay     []byte
	Warnings    []string
	OnExecFail  func() // runs when Exec returns (the exec failed): drops state minted for the launch
}

// Seams for tests.
var (
	ensureRouter = proxy.Ensure
	findClaude   = ClaudeBinary
	lookPath     = paths.VendorBin
	selfBinary   = SelfBinary
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
func selection(o Options) (*providers.Registry, providers.Selection, error) {
	return sessionSelection(o, false)
}

// sessionSelection loads the registry and applies --profile/--model > pins > defaults; session marks a Claude launch.
func sessionSelection(o Options, session bool) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	sel, err := providers.Select(reg, o.Provider)
	if o.Model != "" && sel.Provider != nil {
		applyModel(&sel, o.Model, session, o.Tier)
		err = nil
	}
	return reg, sel, err
}

// MainTier is the tier whose model is the launch's main model: the family --model names, else the verb's tier, else opus.
func MainTier(o Options) string {
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
		dst = MainTier(Options{Model: model, Tier: verbTier})
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
func Claude(ctx context.Context, o Options, args []string) (Plan, error) {
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
	if configDirIsDefault() {
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

// configDirIsDefault: an explicit CLAUDE_CONFIG_DIR=~/.claude makes claude write a stray ~/.claude/.claude.json.
func configDirIsDefault() bool {
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
func configureRouting(ctx context.Context, o Options, reg *providers.Registry, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *Plan) (string, error) {
	helper := shellQuote(helperBin(self)) + " token --profile " + shellQuote(p.Name)
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
		return shellQuote(helperBin(self)) + " token --proxy", nil
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

// Vendor resolves `claude-code run <vendor>`: the opus target via vendors.Configure, else the vendor's own login.
func Vendor(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	plan, err := vendorPlan(ctx, name, o, args)
	plan.Keep, plan.Vendor = o.KeepEnv, true
	return plan, err
}

// withFlagModel re-adds the --model ParseArgs consumed, so the registry selects flag > pin > default.
func withFlagModel(args []string, model string) []string {
	if model == "" {
		return args
	}
	return append([]string{"--model", model}, args...)
}

// withCodexFile prepends the -f file's codex overrides (after the harness prefix, so the file's keys win).
func withCodexFile(name, file string, args []string) ([]string, error) {
	if file == "" {
		return args, nil
	}
	if name != "codex" {
		return nil, fmt.Errorf("-f is codex only (got %s)", name)
	}
	overrides, err := codexFileOverrides(file)
	if err != nil {
		return nil, err
	}
	return append(overrides, args...), nil
}

// agentModelTarget applies flag > the agent's model pin > default to t; a fallback provider respells the flag itself.
func agentModelTarget(name string, t providers.Target, sel providers.Selection, flagModel string) (providers.Target, string) {
	applied := t.Provider == nil || t.Provider == sel.Slots[models.Opus].Provider
	return vendors.ResolveAgentModel(vendors.ModelAsk{Agent: name, Flag: flagModel, Target: t, Applied: applied})
}

func vendorPlan(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	if o.Direct {
		ctx = vendors.WithDirect(ctx)
	}
	spec, ok := vendors.Lookup(name)
	if !ok {
		return Plan{}, fmt.Errorf("unknown agent %q (codex|gemini|opencode|copilot)", name)
	}
	if authVendorArgs(args) {
		return plainPlan(spec, args) // signing in needs no model route; resolving one first blocks the very login it needs
	}
	args, err := withCodexFile(name, o.File, args)
	if err != nil {
		return Plan{}, err
	}
	flagModel := o.Model // the flag alone; the agent's model pin is applied after the target resolves
	o = withAgentPins(name, o)
	reg, sel, err := selection(o)
	if err != nil {
		return Plan{}, err
	}
	// --profile or the agent's own profile pin is explicit: the session pin is Claude Code's default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := vendors.ResolveTarget(reg, name, sel, explicit)
	if err == nil {
		var modelNote string
		if target, modelNote = agentModelTarget(name, target, sel, flagModel); modelNote != "" {
			note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(s string) bool { return s == "" }), "; ")
		}
	}
	// An own-login note is only worth a line on an interactive launch (or --print-env): `opencode --version` stays quiet.
	showOwnLoginNote := o.PrintEnv || (ansi.IsTerminal(os.Stderr) && interactiveVendorArgs(args))
	if errors.Is(err, vendors.ErrOwnLogin) {
		if showOwnLoginNote {
			fmt.Fprintln(os.Stderr, note)
		}
		return plainPlan(spec, args)
	}
	if err != nil {
		return Plan{}, err
	}
	if name == "copilot" && vendors.UsesCopilotRegistry(ctx, target) {
		return copilotRegistryPlan(ctx, spec, reg, target, note, withFlagModel(args, flagModel), o.PrintEnv)
	}
	env, prefix, err := vendors.Configure(ctx, name, target, "")
	if errors.Is(err, vendors.ErrOwnLogin) {
		// The vendor's own login (opencode on Copilot) picks the model itself: plain launch, not a failure.
		if showOwnLoginNote {
			fmt.Fprintln(os.Stderr, note)
		}
		return plainPlan(spec, args)
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	if err != nil {
		reason := fmt.Errorf("%w; %s", err, vendors.ConfigureHint(name))
		// A human at a terminal is offered configure once; a yes re-resolves on the new state.
		if OfferConfigure != nil && !o.PrintEnv && OfferConfigure(name, reason) {
			env, prefix, err = retryVendorConfigure(ctx, name, o)
		}
		if errors.Is(err, vendors.ErrOwnLogin) {
			return plainPlan(spec, args)
		}
		if err != nil {
			return passthrough(spec, name, reason, args)
		}
	}
	bin, err := lookPath(spec.Binary)
	if err != nil {
		return Plan{}, err
	}
	set := envMap(env)
	return Plan{Binary: bin, Set: set, Argv: append(prefix, args...)}, nil
}

// withAgentPins fills the profile the flags left empty from the agent's vault pin: --profile > pin > default.
// The model pin is applied after the target resolves (agentModelTarget).
func withAgentPins(name string, o Options) Options {
	pin, ok := providers.AgentPins()[name]
	if !ok || pin.Off {
		return o
	}
	if o.Provider == "" && pin.Profile != "" {
		if reg, err := providers.Load(); err == nil && providers.ValidAgentPin(reg, name, pin.Profile) == nil && reg.Providers[pin.Profile] != nil {
			o.Provider = pin.Profile
		}
	}
	return o
}

// copilotRegistryPlan is the interactive Databricks copilot launch on a native model registry. It fails closed:
// no error here reaches passthrough or the plain CLI, which would run on copilot's own GitHub login.
func copilotRegistryPlan(
	ctx context.Context, spec vendors.Spec, reg *providers.Registry, t providers.Target, note string, args []string,
	dry bool,
) (Plan, error) {
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	cr, err := vendors.ConfigureCopilotRegistry(ctx, reg, t, args, dry)
	if err != nil {
		return Plan{}, err
	}
	bin, err := lookPath(spec.Binary)
	if err != nil {
		cr.Revoke()
		return Plan{}, err
	}
	lenv, lprefix := vendors.LaunchSpec(spec.Name)
	fmt.Fprintln(os.Stderr, vendors.CopilotRegistryBanner)
	return Plan{
		Binary: bin, Set: envMap(append(lenv, cr.Env...)), Unset: cr.Unset, Warnings: cr.Warnings,
		Argv:       append(append(lprefix, cr.Argv...), cr.Args...),
		OnExecFail: cr.Revoke,
	}, nil
}

func retryVendorConfigure(ctx context.Context, name string, o Options) ([]string, []string, error) {
	reg, sel, err := selection(o)
	if err != nil {
		return nil, nil, err
	}
	target, note, err := vendors.ResolveTarget(reg, name, sel, o.Provider != "")
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return vendors.Configure(ctx, name, target, "")
}

// authVendorArgs reports a vendor's own sign-in subcommand: codex/copilot login|logout, gemini/opencode auth.
func authVendorArgs(args []string) bool {
	return len(args) > 0 && (args[0] == "login" || args[0] == "logout" || args[0] == "auth")
}

func interactiveVendorArgs(args []string) bool {
	if len(args) > 0 && args[0] == "run" {
		return false
	}
	for _, arg := range args {
		switch arg {
		case "--version", "-v", "-V", "--help", "-h":
			return false
		}
	}
	return true
}

// envMap turns a KEY=VALUE env list into a map, dropping malformed entries.
func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Env applies the plan to base (KEY=VALUE list).
func (p Plan) Env(base []string) []string {
	merged, locked := moduleEnv(base)
	return p.envWith(merged, customEnv(), locked)
}

// moduleEnv appends vendorsettings module exports onto base, skipping keys the process env sets; locked names those.
func moduleEnv(base []string) ([]string, map[string]bool) {
	present := map[string]bool{}
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok {
			present[k] = true
		}
	}
	locked := map[string]bool{}
	reg, err := providers.Load()
	if err != nil {
		return base, locked
	}
	out := base
	for _, m := range vendorsettings.Modules() {
		for k, v := range m.Env(reg.Routing) {
			if present[k] {
				locked[k] = true // process env already set this module-owned key: protect it from custom env too
				continue
			}
			out = append(out, k+"="+v)
			present[k] = true
		}
	}
	return out, locked
}

// customEnv is the vault env bucket resolved (@secret refs via secrets.Lookup); an unresolved ref is warned, not
// set — a Lookup error (e.g. a locked vault) also leaves the key out of vals, so a base env value survives.
var customEnv = func() map[string]string {
	errored := map[string]bool{}
	vals, missing, err := providers.ResolveCustomEnv(func(k string) (string, bool) {
		v, _, err := secrets.Lookup(k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: custom env %s: %v; not set\n", k, err)
			errored[k] = true
			return "", false
		}
		return v, v != ""
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: custom env unreadable: %v\n", err)
	}
	for _, k := range missing {
		if errored[k] {
			continue // already warned above with the real error
		}
		fmt.Fprintf(os.Stderr, "claude-code: custom env %s=@secret has no vault value; not set\n", k)
	}
	return vals
}

// envWith layers base, then custom (user vars), then Set; Unset and Set beat custom, locked keys skip custom.
func (p Plan) envWith(base []string, custom map[string]string, locked map[string]bool) []string {
	unset := map[string]bool{}
	for _, k := range p.Unset {
		unset[k] = true
	}
	merged := maps.Clone(p.Set)
	if merged == nil {
		merged = map[string]string{}
	}
	for k, v := range custom {
		_, harness := p.Set[k]
		if !harness && !unset[k] && !locked[k] {
			merged[k] = v
		}
	}
	p.Set = merged
	return p.plainEnv(base)
}

// plainEnv is the credential scrub, then base minus Unset/Set keys, plus Set.
func (p Plan) plainEnv(base []string) []string {
	if p.Vendor {
		base, _ = envscrub.ScrubVendor(base, envscrub.KeepSet(p.Keep))
	} else {
		base, _ = envscrub.Scrub(base, envscrub.KeepSet(p.Keep))
	}
	drop := map[string]bool{}
	for _, k := range p.Unset {
		drop[k] = true
	}
	for k := range p.Set {
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(p.Set))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		out = append(out, k+"="+p.Set[k])
	}
	return out
}

// Print writes the resolved launch; header values are masked since static headers may carry secrets.
func (p Plan) Print(w io.Writer) {
	fmt.Fprintf(w, "binary   %s\nargv     %s\n", p.Binary, redactQuoted(strings.Join(p.Argv, " ")))
	for _, k := range p.unsetNames() {
		fmt.Fprintf(w, "unset    %s\n", k)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		v := p.Set[k]
		switch {
		case secretKey(k):
			v = "<redacted>"
		case k == "OPENCODE_CONFIG_CONTENT":
			v = redactQuoted(vendors.RedactConfig(v))
		}
		fmt.Fprintf(w, "set      %s=%s\n", k, v)
	}
	for _, k := range slices.Sorted(maps.Keys(providers.CustomEnv())) {
		switch _, harness := p.Set[k]; {
		case harness || slices.Contains(p.Unset, k):
			fmt.Fprintf(w, "custom   %s (overridden by the launch)\n", k)
		default:
			fmt.Fprintf(w, "custom   %s (vault env)\n", k)
		}
	}
	if len(p.Overlay) > 0 {
		fmt.Fprintf(w, "overlay  %s\n%s\n", p.OverlayPath, p.Overlay)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warn)
	}
}

// unsetNames is every inherited var this plan drops from the real environment: the fixed Anthropic
// wipe list plus whatever the credential scrub catches, minus anything the plan itself sets.
func (p Plan) unsetNames() []string {
	names := map[string]bool{}
	for _, k := range p.Unset {
		if _, set := p.Set[k]; !set && os.Getenv(k) != "" {
			names[k] = true
		}
	}
	scrub := envscrub.Scrub
	if p.Vendor {
		scrub = envscrub.ScrubVendor
	}
	_, dropped := scrub(os.Environ(), envscrub.KeepSet(p.Keep))
	for _, k := range dropped {
		if _, set := p.Set[k]; !set {
			names[k] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

// WriteOverlay writes the --settings overlay the argv names, when the plan has one.
func (p Plan) WriteOverlay() error {
	if len(p.Overlay) == 0 {
		return nil
	}
	return paths.AtomicWrite(p.OverlayPath, p.Overlay, 0o600)
}

// Exec writes the overlay and replaces this process with the target.
func (p Plan) Exec() error {
	if err := p.WriteOverlay(); err != nil {
		p.execFailed()
		return err
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(os.Stderr, "claude-code: warning:", w)
	}
	err := proc.Exec(p.Binary, p.Argv, p.Env(os.Environ()))
	p.execFailed()
	return err
}

func (p Plan) execFailed() {
	if p.OnExecFail != nil {
		p.OnExecFail()
	}
}

func secretKey(k string) bool {
	return secretKeyRE.MatchString(k)
}

// secretKeyRE matches the same credential-shaped name segments as providers' validateEnv.
var secretKeyRE = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|HEADERS|AUTH|CREDENTIALS?|PAT|BEARER)(_|$)`)

// quotedRE is one double-quoted string; codex -c TOML and opencode JSON carry static provider headers in them.
var quotedRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// redactQuoted masks every credential-shaped quoted string in s.
func redactQuoted(s string) string {
	return quotedRE.ReplaceAllStringFunc(s, func(q string) string {
		inner := q[1 : len(q)-1]
		if shapes.Looks(inner) || shapes.Redact(inner) != inner {
			return `"<redacted>"`
		}
		return q
	})
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

// OfferConfigure is the interactive seam `configure` installs: asked once when a vendor cannot be routed, it
// may set up that vendor (login or key) and returns true when the launch should re-resolve. nil = no offer.
var OfferConfigure func(vendor string, reason error) bool

// passthrough launches the vendor's own CLI untouched when the harness cannot route it: the user installed
// that CLI and it carries its own login, so refusing to run it would only hide its own `agy` sign-in.
func passthrough(spec vendors.Spec, name string, reason error, args []string) (Plan, error) {
	if _, err := lookPath(spec.Binary); err != nil {
		return Plan{}, reason // no CLI to fall back to: the routing error is the whole story
	}
	fmt.Fprintf(os.Stderr, "%v\nlaunching the plain %s CLI on its own login (not routed by claude-code)\n", reason, name)
	return plainPlan(spec, args)
}

// plainPlan is the vendor's own CLI on its own login: no routing env, but the vendor spec's launch env/args still apply.
func plainPlan(spec vendors.Spec, args []string) (Plan, error) {
	bin, err := lookPath(spec.Binary)
	if err != nil {
		return Plan{}, err
	}
	env, prefix := vendors.LaunchSpec(spec.Name)
	return Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...)}, nil
}
