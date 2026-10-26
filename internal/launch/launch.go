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
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/vendors"
	"github.com/edotau/claude-code/internal/vendorsettings"
)

// Options are the harness flags consumed before passthrough.
type Options struct {
	Provider string
	Model    string // [provider:]model for the opus slot, this launch only
	Tier     string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	Router   bool
	PrintEnv bool
	Direct   bool     // vendors only: skip the router, hand the CLI the provider route
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
	File     string   // codex only: a config.toml whose keys ride as -c overrides after the harness's, from -f
	// ModelSlot is the slot --model fills ("" = opus); only the anthropic verb sets it.
	ModelSlot string
}

// ParseArgs strips harness flags from args; the first "--" ends harness parsing and is dropped.
func ParseArgs(args []string, allowRouter bool) (Options, []string, error) {
	var o Options
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			return o, append(rest, args[i+1:]...), nil
		case name == "--provider" || name == "--model":
			if !hasVal {
				if i+1 >= len(args) {
					return o, nil, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			if name == "--provider" {
				o.Provider = val
			} else {
				o.Model = val
			}
		case a == "--router" && allowRouter:
			o.Router = true
		case a == "--direct" && !allowRouter:
			o.Direct = true
		case a == "--print-env":
			o.PrintEnv = true
		case (name == "-f" || name == "--file") && !allowRouter:
			if !hasVal {
				if i+1 >= len(args) {
					return o, nil, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
			}
			o.File = val
		case name == "--keep-env":
			if !hasVal {
				if i+1 >= len(args) {
					return o, nil, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = args[i]
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
	Profile     string // active profile whose session row applied ("" = none)
}

// Seams for tests.
var (
	ensureRouter = router.Ensure
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
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_GATEWAY",
	"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CLAUDE_CODE_ANTHROPIC_MODEL", "OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
}

// oauthKey survives the wipe only for passthrough (subscription) providers.
const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// selection loads the registry and applies the per-launch provider/model flags.
func selection(o Options) (*providers.Registry, providers.Selection, error) {
	reg, sel, _, err := sessionSelection(o, false)
	return reg, sel, err
}

// sessionSelection is selection plus, when session is set, the active profile's session row:
// --provider/--model > row > pins > defaults. The returned name is the profile that applied ("" = none).
func sessionSelection(o Options, session bool) (*providers.Registry, providers.Selection, string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, "", err
	}
	var applied string
	var slots map[string]string
	if session {
		bd := reg.ResolveAgent(providers.AgentRequest{Agent: "session", Provider: o.Provider, Model: o.Model})
		applied, slots, o.Provider = bd.Profile, bd.Slots, bd.Provider
	}
	sel, err := providers.Select(reg, o.Provider)
	if applied != "" && sel.Provider != nil {
		for slot, spec := range slots {
			sel.Slots[slot] = providers.ParseTarget(reg, spec, sel.Provider)
		}
		if _, ok := sel.Slots[models.Opus]; ok {
			err = nil
		}
	}
	if o.Model != "" && sel.Provider != nil {
		sel.Slots[cmp.Or(o.ModelSlot, models.Opus)] = providers.ParseTarget(reg, o.Model, sel.Provider)
		err = nil
	}
	return reg, sel, applied, err
}

// Claude resolves a Claude Code launch.
func Claude(ctx context.Context, o Options, args []string) (Plan, error) {
	reg, sel, profile, err := sessionSelection(o, true)
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
	passthrough := p.Auth.Type == providers.AuthPassthrough
	if p.Auth.Login != "" {
		return Plan{}, fmt.Errorf("provider %s is the %s sign-in and cannot back Claude Code; use claude-code run %s --provider %s", p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	plan := Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), wipeKeys...), Profile: profile}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
	}
	if configDirIsDefault() {
		plan.Unset = append(plan.Unset, "CLAUDE_CONFIG_DIR")
	}
	setModelSlotEnv(&plan, sel)
	main := models.Opus
	if o.Tier != "" {
		main = o.Tier
	}
	if err := setMainModel(&plan, sel, p, main); err != nil {
		return Plan{}, err
	}
	plan.Set[providers.PinProvider] = p.Name
	helper, err := configureRouting(ctx, o, reg, sel, p, passthrough, self, &plan)
	if err != nil {
		return Plan{}, err
	}
	applyOverlay(&plan, passthrough, args, helper, p.Env)
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

// setMainModel resolves the main-loop model for tier and sets ANTHROPIC_MODEL + thinking knobs.
func setMainModel(plan *Plan, sel providers.Selection, p *providers.Provider, tier string) error {
	opus := sel.ClientModel(tier)
	if opus == "" {
		return fmt.Errorf("provider %s has no %s model; pin one with claude-code models --pin %s=<id>", p.Name, tier, tier)
	}
	plan.Set["ANTHROPIC_MODEL"] = opus
	if maxTok, disable := models.ThinkingKnobs(opus); maxTok != "" {
		plan.Set["MAX_THINKING_TOKENS"] = maxTok
		if disable != "" {
			plan.Set["CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"] = disable
		}
	}
	return nil
}

// configureRouting wires the router base URL/headers when needed, else the provider's own route, and
// returns the apiKeyHelper command for the overlay.
func configureRouting(ctx context.Context, o Options, reg *providers.Registry, sel providers.Selection, p *providers.Provider, passthrough bool, self string, plan *Plan) (string, error) {
	helper := shellQuote(helperBin(self)) + " token --provider " + shellQuote(p.Name)
	if o.Router || sel.NeedsRouter(reg) {
		for _, t := range sel.Slots {
			if t.Provider.Auth.Type == providers.AuthPassthrough && !passthrough {
				return "", fmt.Errorf("slot on %s: subscription (passthrough) auth needs a passthrough session provider — claude-code switch %s, or pick an API-key provider", t.Provider.Name, t.Provider.Name)
			}
		}
		base, err := ensureRouter(ctx)
		if err != nil {
			return "", fmt.Errorf("router: %w", err)
		}
		plan.Set["ANTHROPIC_BASE_URL"] = router.SessionBase(base, p.Name)
		if passthrough {
			// The login stays in Authorization, so the router secret rides in its own header.
			secret, err := router.ClientSecret()
			if err != nil {
				return "", fmt.Errorf("router secret: %w", err)
			}
			plan.Set["ANTHROPIC_CUSTOM_HEADERS"] = router.ClientHeader + ": " + secret
		}
		plan.OverlayPath = filepath.Join(paths.StateDir(), "overlay-"+p.Name+"-router.json")
		return shellQuote(helperBin(self)) + " token --router", nil
	}
	if route, _ := p.Route(providers.DialectAnthropic); route != "https://api.anthropic.com" {
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
func applyOverlay(plan *Plan, passthrough bool, args []string, helper string, env map[string]string) {
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
		return
	}
	plan.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = "300000"
	plan.Overlay, _ = json.MarshalIndent(map[string]string{"apiKeyHelper": helper}, "", "  ")
	plan.Argv = []string{"--settings", plan.OverlayPath}
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

// Vendor resolves `claude-code run <vendor>`: the opus target wired through vendors.Configure, falling
// back to the vendor's own subscription login or a keyed provider when the session provider cannot
// serve it and the caller named no provider explicitly.
func Vendor(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	plan, err := vendorPlan(ctx, name, o, args)
	plan.Keep, plan.Vendor = o.KeepEnv, true
	return plan, err
}

// vendorProfile applies the active profile's row for the vendor to o (flags win) and names the profile.
func vendorProfile(name string, o Options) (Options, string) {
	reg, err := providers.Load()
	if err != nil {
		return o, ""
	}
	b := reg.ResolveAgent(providers.AgentRequest{Agent: name, Provider: o.Provider, Model: o.Model})
	if b.Profile == "" {
		return o, ""
	}
	o.Provider, o.Model = b.Provider, b.Model
	return o, b.Profile
}

func vendorPlan(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	if o.Direct {
		ctx = vendors.WithDirect(ctx)
	}
	spec, ok := vendors.Lookup(name)
	if !ok {
		return Plan{}, fmt.Errorf("unknown vendor %q (codex|gemini|opencode|copilot)", name)
	}
	if authVendorArgs(args) {
		return plainPlan(spec, args) // signing in needs no model route; resolving one first blocks the very login it needs
	}
	if o.File != "" {
		if name != "codex" {
			return Plan{}, fmt.Errorf("-f is codex only (got %s)", name)
		}
		overrides, err := codexFileOverrides(o.File)
		if err != nil {
			return Plan{}, err
		}
		args = append(overrides, args...) // after the harness prefix, so the file's keys win
	}
	o, profile := vendorProfile(name, o)
	reg, sel, err := selection(o)
	if err != nil {
		return Plan{}, err
	}
	// Only --provider is explicit: the session pin (env or file) is Claude Code's default, not a per-vendor choice.
	explicit := o.Provider != ""
	target, note, err := vendors.ResolveTarget(reg, name, sel, explicit)
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
	return Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...), Profile: profile}, nil
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

// moduleEnv appends every vendorsettings module's export onto base, skipping any key base (the process
// environment) already sets — process env wins, pins-style. The returned locked set names every
// module-owned key the process environment already set: envWith must not let a vault custom-env entry
// override those, so the full precedence is process env > vault custom env > module env. A registry load
// error exports nothing and locks nothing.
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

// envWith layers base, then custom (user vars), then Set; Unset and Set keys always beat a custom var.
// locked names module-owned keys the process environment already set: those stay out of the custom
// merge entirely, so process always beats a vault custom-env entry for them.
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
	if p.Profile != "" {
		fmt.Fprintf(w, "profile  %s (session row applied)\n", p.Profile)
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
		return err
	}
	for _, w := range p.Warnings {
		fmt.Fprintln(os.Stderr, "claude-code: warning:", w)
	}
	return proc.Exec(p.Binary, p.Argv, p.Env(os.Environ()))
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
