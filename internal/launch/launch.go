// Package launch builds and execs Claude Code and vendor agent CLIs against the provider registry.
package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vendors"
)

// Options are the harness flags consumed before passthrough.
type Options struct {
	Provider string
	Model    string // [provider:]model for the opus slot, this launch only
	Router   bool
	PrintEnv bool
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
	plan := Plan{Binary: bin, Set: map[string]string{}, Unset: append([]string(nil), wipeKeys...)}
	if !passthrough {
		plan.Unset = append(plan.Unset, oauthKey)
	}
	for _, slot := range models.Slots {
		if m := sel.ClientModel(slot); m != "" {
			plan.Set["ANTHROPIC_DEFAULT_"+strings.ToUpper(slot)+"_MODEL"] = m
		}
	}
	opus := sel.ClientModel(models.Opus)
	plan.Set["ANTHROPIC_MODEL"] = opus
	if maxTok, disable := models.ThinkingKnobs(opus); maxTok != "" {
		plan.Set["MAX_THINKING_TOKENS"] = maxTok
		if disable != "" {
			plan.Set["CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING"] = disable
		}
	}
	plan.Set[providers.PinProvider] = p.Name
	helper := shellQuote(self) + " token --provider " + shellQuote(p.Name)
	if o.Router || sel.NeedsRouter(reg) {
		for _, t := range sel.Slots {
			if t.Provider.Auth.Type == providers.AuthPassthrough && !passthrough {
				return Plan{}, fmt.Errorf("slot on %s: subscription (passthrough) auth needs a passthrough session provider — claude-code use %s, or pick an API-key provider", t.Provider.Name, t.Provider.Name)
			}
		}
		base, err := ensureRouter(ctx)
		if err != nil {
			return Plan{}, fmt.Errorf("router: %w", err)
		}
		plan.Set["ANTHROPIC_BASE_URL"] = router.SessionBase(base, p.Name)
		if passthrough {
			// The login stays in Authorization, so the router secret rides in its own header.
			secret, err := router.ClientSecret()
			if err != nil {
				return Plan{}, fmt.Errorf("router secret: %w", err)
			}
			plan.Set["ANTHROPIC_CUSTOM_HEADERS"] = router.ClientHeader + ": " + secret
		}
		helper = shellQuote(self) + " token --router"
		plan.OverlayPath = filepath.Join(paths.StateDir(), "overlay-"+p.Name+"-router.json")
	} else {
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
	}
	if passthrough {
		plan.OverlayPath = "" // Claude Code disables subscription OAuth whenever an apiKeyHelper is set
	} else {
		plan.Set["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] = "300000"
		plan.Overlay, _ = json.MarshalIndent(map[string]string{"apiKeyHelper": helper}, "", "  ")
		plan.Argv = []string{"--settings", plan.OverlayPath}
	}
	plan.Argv = append(plan.Argv, args...)
	return plan, nil
}

// Vendor resolves `claude-code run <vendor>`: the opus target wired through vendors.Configure.
func Vendor(ctx context.Context, name string, o Options, args []string) (Plan, error) {
	spec, ok := vendors.Lookup(name)
	if !ok {
		return Plan{}, fmt.Errorf("unknown vendor %q (codex|gemini|opencode|copilot)", name)
	}
	_, sel, err := selection(o)
	if err != nil {
		return Plan{}, err
	}
	env, prefix, err := vendors.Configure(ctx, name, sel.Slots[models.Opus], "")
	if err != nil {
		return Plan{}, err
	}
	bin, err := lookPath(spec.Binary)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Binary: bin, Set: map[string]string{}, Argv: append(prefix, args...)}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			plan.Set[k] = v
		}
	}
	return plan, nil
}

// Env applies the plan to base (KEY=VALUE list).
func (p Plan) Env(base []string) []string {
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
	for _, k := range p.Unset {
		if _, set := p.Set[k]; !set && os.Getenv(k) != "" {
			fmt.Fprintf(w, "unset    %s\n", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Set)) {
		v := p.Set[k]
		if secretKey(k) {
			v = "<redacted>"
		}
		fmt.Fprintf(w, "set      %s=%s\n", k, v)
	}
	if len(p.Overlay) > 0 {
		fmt.Fprintf(w, "overlay  %s\n%s\n", p.OverlayPath, p.Overlay)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warn)
	}
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
