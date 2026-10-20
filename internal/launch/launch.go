// Package launch builds and execs Claude Code and vendor agent CLIs against the provider registry.
package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
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
	"github.com/edotau/claude-code/internal/vendors"
)

// Options are the harness flags consumed before passthrough.
type Options struct {
	Provider string
	Model    string // [provider:]model for the opus slot, this launch only
	Tier     string // opus|sonnet|haiku|fable: the main-loop model; the other slots keep their own pins
	Router   bool
	PrintEnv bool
	KeepEnv  []string // names the credential scrub must not drop, from --keep-env
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
		case a == "--print-env":
			o.PrintEnv = true
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
	OverlayPath string
	Overlay     []byte
	Warnings    []string
}

// Seams for tests.
var (
	ensureRouter = router.Ensure
	findClaude   = ClaudeBinary
	lookPath     = paths.LookPathReal
	selfBinary   = SelfBinary
)

// wipeKeys are inherited Anthropic knobs that would override the launch's own resolution.
var wipeKeys = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
	"MAX_THINKING_TOKENS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING", "ANTHROPIC_CUSTOM_HEADERS",
}

// oauthKey survives the wipe only for passthrough (subscription) providers.
const oauthKey = "CLAUDE_CODE_OAUTH_TOKEN"

// selection loads the registry and applies the per-launch provider/model flags.
func selection(o Options) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	sel, err := providers.Select(reg, o.Provider)
	if o.Model != "" && sel.Provider != nil {
		sel.Slots[models.Opus] = providers.ParseTarget(reg, o.Model, sel.Provider)
		err = nil
	}
	return reg, sel, err
}

// Claude resolves a Claude Code launch.
func Claude(ctx context.Context, o Options, args []string) (Plan, error) {
	reg, sel, err := selection(o)
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
	plan := Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), wipeKeys...)}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
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
	applyOverlay(&plan, passthrough, args, helper)
	plan.Argv = append(plan.Argv, args...)
	plan.Keep = o.KeepEnv
	return plan, nil
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
	helper := shellQuote(self) + " token --provider " + shellQuote(p.Name)
	if o.Router || sel.NeedsRouter(reg) {
		for _, t := range sel.Slots {
			if t.Provider.Auth.Type == providers.AuthPassthrough && !passthrough {
				return "", fmt.Errorf("slot on %s: subscription (passthrough) auth needs a passthrough session provider — claude-code use %s, or pick an API-key provider", t.Provider.Name, t.Provider.Name)
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
		return shellQuote(self) + " token --router", nil
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
func applyOverlay(plan *Plan, passthrough bool, args []string, helper string) {
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

// headless reports a `claude -p`/`--print` run, the only launch that carries the setup-token.
func headless(args []string) bool {
	return slices.Contains(args, "-p") || slices.Contains(args, "--print")
}

// Vendor resolves `claude-code run <vendor>`: the opus target wired through vendors.Configure, falling
// back to the vendor's own subscription login or a keyed provider when the session provider cannot
// serve it and the caller named no provider explicitly.
func Vendor(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	plan, err := vendorPlan(ctx, name, o, args)
	plan.Keep = o.KeepEnv
	return plan, err
}

func vendorPlan(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	spec, ok := vendors.Lookup(name)
	if !ok {
		return Plan{}, fmt.Errorf("unknown vendor %q (codex|gemini|opencode|copilot)", name)
	}
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
	return Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...)}, nil
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
	return p.envWith(base, customEnv())
}

// customEnv is the vault env bucket resolved (keyring refs via secrets.Get); an unresolved ref is warned, not set.
var customEnv = func() map[string]string {
	vals, missing := providers.ResolveCustomEnv(func(k string) (string, bool) {
		v, _, ok := secrets.Get(k)
		return v, ok
	})
	for _, k := range missing {
		fmt.Fprintf(os.Stderr, "claude-code: custom env %s=@keyring has no keyring value; not set\n", k)
	}
	return vals
}

// envWith layers base, then custom (user vars), then Set; Unset and Set keys always beat a custom var.
func (p Plan) envWith(base []string, custom map[string]string) []string {
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
		if !harness && !unset[k] {
			merged[k] = v
		}
	}
	p.Set = merged
	return p.plainEnv(base)
}

// plainEnv is the credential scrub, then base minus Unset/Set keys, plus Set.
func (p Plan) plainEnv(base []string) []string {
	base, _ = envscrub.Scrub(base, envscrub.KeepSet(p.Keep))
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
	fmt.Fprintf(w, "binary   %s\nargv     %s\n", p.Binary, strings.Join(p.Argv, " "))
	for _, k := range p.unsetNames() {
		fmt.Fprintf(w, "unset    %s\n", k)
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		v := p.Set[k]
		switch {
		case secretKey(k):
			v = "<redacted>"
		case k == "OPENCODE_CONFIG_CONTENT":
			v = vendors.RedactConfig(v)
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
	_, dropped := envscrub.Scrub(os.Environ(), envscrub.KeepSet(p.Keep))
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
	for _, suf := range []string{"_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_HEADERS"} {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
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
