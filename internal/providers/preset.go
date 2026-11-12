package providers

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// PresetDatabricks derives every route, the auth commands and the launch env from one host + one CLI profile.
const PresetDatabricks = "databricks"

var (
	cliProfileRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	EnvNameRE    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`) // an env-var name; also the vault key-name shape
	secretNameRE = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|HEADERS|AUTH|CREDENTIALS?|PAT|BEARER)(_|$)`)
)

// IsSecretName reports whether an env name has a credential-shaped segment (KEY, TOKEN, AUTH, ...).
func IsSecretName(name string) bool { return secretNameRE.MatchString(name) }

// DatabricksProfiles lists the profiles under the databricks provider in declared order.
func DatabricksProfiles(r *Registry) []string { return r.ProfilesOf(GroupDatabricks) }

// BetaOffKey is the launch env key that disables Claude Code's experimental betas.
const BetaOffKey = "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"

// databricksModels are the canonical ids a databricks preset seeds into empty aliases.
var databricksModels = map[string]string{
	"opus": "claude-opus-5-5", "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5",
}

// expandPresets fills each preset provider's empty fields; load-only. Invalid presets are left for Validate.
func expandPresets(r *Registry) {
	for _, p := range r.Providers {
		if p == nil || p.Preset != PresetDatabricks || PresetHost(p.Host) == "" {
			continue
		}
		expandDatabricks(p)
	}
}

// decorateModels rewrites every provider's alias ids to wire ids in memory; load-only, never persisted.
func decorateModels(r *Registry) {
	for _, p := range r.Providers {
		if p == nil || len(p.Models) == 0 || p.SpellingRule() == "" {
			continue
		}
		out := make(map[string]string, len(p.Models))
		for alias, id := range p.Models {
			out[alias] = p.WireModel(id)
		}
		p.Models = out
	}
}

// PresetHost is oauth.NormalizeHost for an absolute https URL only: a bare host is "".
func PresetHost(host string) string {
	if !strings.Contains(host, "://") {
		return ""
	}
	return oauth.NormalizeHost(host)
}

func expandDatabricks(p *Provider) {
	host := PresetHost(p.Host)
	if p.Protocol == "" {
		p.Protocol = ProtocolAnthropic
	}
	if p.isGateway() {
		expandGateway(p, host)
	} else {
		expandServing(p, host)
	}
	if p.Auth.Type == "" {
		p.Auth.Type = AuthKey
	}
	if p.Auth.TTL == 0 {
		p.Auth.TTL = 1800
	}
	fillDatabricksModelsEnv(p)
	applyCLIProfile(p)
}

// fillDatabricksModelsEnv fills the default model aliases and the beta-off env key where unset.
func fillDatabricksModelsEnv(p *Provider) {
	for alias, id := range databricksModels {
		if p.Models[alias] == "" {
			p.Models = withKey(p.Models, alias, id)
		}
	}
	if p.Env[BetaOffKey] == "" {
		p.Env = withKey(p.Env, BetaOffKey, "1")
	}
}

// applyCLIProfile derives auth.command / auth.refreshCommand from a well-formed cliProfile where unset.
func applyCLIProfile(p *Provider) {
	if p.CLIProfile == "" || !cliProfileRE.MatchString(p.CLIProfile) {
		return
	}
	cmd, refresh := DatabricksCLICommands(p.CLIProfile)
	if p.Auth.Command == "" {
		p.Auth.Command = cmd
	}
	if p.Auth.RefreshCommand == "" {
		p.Auth.RefreshCommand = refresh
	}
}

// DatabricksCLICommands are the auth.command / auth.refreshCommand a cliProfile derives.
func DatabricksCLICommands(profile string) (command, refresh string) {
	mint := "databricks auth token -p " + profile
	return tokenFromCLI(mint + " -o json"), tokenFromCLI(mint + " --force-refresh -o json")
}

// tokenFromCLI pipes a CLI's JSON through jq; capturing first makes a CLI failure stop the chain (sh has no pipefail).
func tokenFromCLI(cli string) string {
	return `out=$(` + cli + `) && printf '%s' "$out" | jq -r .access_token`
}

// ValidID reports whether s is a legal provider row id / cliProfile name.
func ValidID(s string) bool { return cliProfileRE.MatchString(s) }

func expandServing(p *Provider, host string) {
	if p.BaseURL == "" {
		p.BaseURL = host + "/serving-endpoints/anthropic"
	}
	for _, d := range []string{ProtocolOpenAI, ProtocolResponses} {
		if p.Routes[d] == "" {
			p.Routes = withKey(p.Routes, d, host+"/serving-endpoints")
		}
	}
	if p.ModelPrefix == "" {
		p.ModelPrefix = models.DatabricksPrefix
	}
}

// expandGateway derives the AI Gateway wire: Claude on /ai-gateway/anthropic, responses on /ai-gateway/codex/v1.
func expandGateway(p *Provider, host string) {
	if p.BaseURL == "" {
		p.BaseURL = host + "/ai-gateway/anthropic"
	}
	if p.Routes[ProtocolResponses] == "" {
		p.Routes = withKey(p.Routes, ProtocolResponses, host+"/ai-gateway/codex/v1")
	}
	if p.ModelPrefix == "" {
		p.ModelPrefix = gatewayModelPrefix
	}
	if p.Headers[gatewayModeHeader] == "" {
		p.Headers = withKey(p.Headers, gatewayModeHeader, "true")
	}
}

// withKey returns a copy of m with k=v.
func withKey(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	maps.Copy(out, m)
	out[k] = v
	return out
}

// validatePreset checks the preset, host and cliProfile fields of one provider.
func validatePreset(n string, p *Provider) []string {
	var errs []string
	if p.Preset == "" {
		if p.Host != "" || p.CLIProfile != "" {
			errs = append(errs, n+": host/cliProfile need a preset")
		}
		return errs
	}
	if p.Preset != PresetDatabricks {
		return append(errs, fmt.Sprintf("%s: unknown preset %q (known: %s)", n, p.Preset, PresetDatabricks))
	}
	return append(errs, validateDatabricksFields(n, p)...)
}

// validateDatabricksFields checks the host, cliProfile and wire of a databricks-preset provider.
func validateDatabricksFields(n string, p *Provider) []string {
	var errs []string
	if PresetHost(p.Host) == "" {
		errs = append(errs, fmt.Sprintf("%s: preset %s needs host as an absolute https URL with no path", n, p.Preset))
	}
	if p.CLIProfile != "" && !cliProfileRE.MatchString(p.CLIProfile) {
		errs = append(errs, fmt.Sprintf("%s: cliProfile %q must match [A-Za-z0-9_-]+", n, p.CLIProfile))
	}
	if w := p.Options.Wire; w != "" && w != WireServing && w != WireAIGateway {
		errs = append(errs, fmt.Sprintf("%s: options.wire %q must be %s or %s", n, w, WireServing, WireAIGateway))
	}
	return errs
}

// WipeKeys are inherited Anthropic knobs that override a Claude launch's own resolution; profile env may not set them.
var WipeKeys = append(slices.Clone(proc.AnthropicStripEnv), []string{
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
	"MAX_THINKING_TOKENS", "CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING",
	"CLAUDE_CODE_DISABLE_1M_CONTEXT", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_GATEWAY",
	"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "CLAUDE_CODE_ANTHROPIC_MODEL", "OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
	EnvSessionProfile, "CLAUDE_MODEL",
	"CLAUDE_OPUS_MODEL", "CLAUDE_SONNET_MODEL", "CLAUDE_HAIKU_MODEL", "CLAUDE_FABLE_MODEL",
}...)

// harnessEnvRE and harnessEnvKeys add the harness-owned names WipeKeys does not list.
var (
	harnessEnvRE   = regexp.MustCompile(`^(ANTHROPIC_|HARNESS_)`)
	harnessEnvKeys = []string{"CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
)

// validateEnv checks the launch env keys: well-formed names, and none credential-shaped (secrets belong in auth).
func validateEnv(n string, env map[string]string) []string {
	var errs []string
	for k := range env {
		switch {
		case !EnvNameRE.MatchString(k):
			errs = append(errs, fmt.Sprintf("%s: env key %q must match [A-Z_][A-Z0-9_]*", n, k))
		case IsSecretName(k):
			errs = append(errs, fmt.Sprintf("%s: env key %q looks like a credential; secrets belong in auth", n, k))
		case harnessEnvRE.MatchString(k) || slices.Contains(harnessEnvKeys, k) || slices.Contains(WipeKeys, k):
			errs = append(errs, fmt.Sprintf("%s: env key %q is owned by the harness", n, k))
		}
	}
	return errs
}

// jwtIssuerHost returns the host of a JWT credential's iss claim; false for anything else.
func jwtIssuerHost(v string) (string, bool) {
	var claims struct {
		Iss string `json:"iss"`
	}
	if !textutil.JWTClaims(v, &claims) || claims.Iss == "" {
		return "", false
	}
	u, err := url.Parse(claims.Iss)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Host, true
}

// HostMismatchError is a credential issued for another host than the route it would be sent to.
type HostMismatchError struct{ msg string }

func (e *HostMismatchError) Error() string { return e.msg }

// checkIssuer fails a preset provider's minted JWT whose issuer host is not the preset host; never names the token.
func checkIssuer(p *Provider, v string) error {
	want := strings.TrimPrefix(PresetHost(p.Host), "https://")
	if p.Preset == "" || want == "" {
		return nil
	}
	if iss, ok := jwtIssuerHost(v); ok && iss != want {
		return &HostMismatchError{fmt.Sprintf("%s: token issuer host %s does not match preset host %s (wrong cliProfile?)", p.Name, iss, want)}
	}
	return nil
}

// CheckRouteHost fails a preset provider whose credential v was issued for a host other than its routes.
func CheckRouteHost(p *Provider, v string) error {
	iss, ok := jwtIssuerHost(v)
	if p == nil || p.Preset == "" || !ok {
		return nil
	}
	for _, d := range p.Dialects() {
		route, _ := p.Route(d)
		if u, err := url.Parse(route); err == nil && u.Host != "" && u.Host != iss {
			return &HostMismatchError{fmt.Sprintf("%s: credential issued for %s, but the %s route is on %s", p.Name, iss, d, u.Host)}
		}
	}
	return nil
}

// Databricks preset wires: serving (default) is /serving-endpoints, ai-gateway is the AI Gateway.
const (
	WireServing   = "serving"
	WireAIGateway = "ai-gateway"
)

// Options are a preset row's non-secret tuning knobs.
type Options struct {
	Wire string `json:"wire,omitempty"` // "" or serving | ai-gateway
}

// gatewayModelPrefix and gatewayModeHeader are the AI Gateway wire-id prefix and the coding-agent header.
const (
	gatewayModelPrefix = "system.ai."
	gatewayModeHeader  = "x-databricks-use-coding-agent-mode"
)

// isGateway reports whether p's preset row selects the AI Gateway wire.
func (p *Provider) isGateway() bool { return p.Options.Wire == WireAIGateway }

// IsAIGateway reports whether p is a Databricks preset row on the AI Gateway wire.
func (p *Provider) IsAIGateway() bool { return p.Preset == PresetDatabricks && p.isGateway() }

// SpellingRule is the models.Wire rule p's model ids use; "" means ids pass through as written.
func (p Provider) SpellingRule() string {
	switch {
	case p.Preset == PresetDatabricks && p.isGateway(), p.ModelPrefix == gatewayModelPrefix: // prefix: rows built without options.wire
		return "databricks-ai-gateway"
	case p.Preset == PresetDatabricks || p.ModelPrefix == models.DatabricksPrefix:
		return "databricks-serving"
	case p.Provider == "github":
		return "dotted"
	case p.Provider == "anthropic":
		return "anthropic"
	}
	return ""
}

// WireModel spells a catalog model id the way p serves it; idempotent, keeps a trailing [1m], and leaves
// non-catalog and provider-qualified ids as written.
func (p Provider) WireModel(id string) string {
	base, oneM := strings.CutSuffix(id, oneMMarker)
	rule := p.SpellingRule()
	if rule == "" || base == "" || strings.ContainsAny(base, "/:") {
		return id
	}
	canonical, _, ok := models.Resolve(base)
	if !ok {
		return id
	}
	if oneM {
		return models.Wire(rule, canonical) + oneMMarker
	}
	return models.Wire(rule, canonical)
}

// Serves1M reports whether p's endpoint serves the 1M window; the endpoint type alone decides.
func (p *Provider) Serves1M() bool {
	if p == nil {
		return false
	}
	switch p.SpellingRule() {
	case "databricks-serving", "anthropic":
		return true
	}
	return false
}

// ContextWindow is the window Claude Code runs id at on p: 1M when typed [1m] or a catalog Claude model on a 1M endpoint.
func ContextWindow(p *Provider, id string) int {
	if strings.HasSuffix(id, oneMMarker) || p.Serves1M() {
		return models.ContextWindow(id)
	}
	return models.ContextWindowStd
}

// ServingRow checks name is a Databricks serving row (databricks:<p> runs on no other); errors never quote name.
func ServingRow(name string) error {
	reg, err := Load()
	if err != nil {
		return fmt.Errorf("overlay profile: %w", err)
	}
	return servingRow(reg.Providers[name], "overlay profile")
}

// ServingRow is the package ServingRow against an already loaded r; its errors name the row (brief lines).
func (r *Registry) ServingRow(name string) error {
	return servingRow(r.Providers[name], "overlay databricks:"+name)
}

func servingRow(p *Provider, label string) error {
	switch {
	case p == nil:
		return fmt.Errorf("%s: no such profile", label)
	case p.Preset != PresetDatabricks:
		return fmt.Errorf("%s: not a Databricks profile", label)
	case p.IsAIGateway():
		return fmt.Errorf("%s: AI Gateway row; use a serving row", label)
	}
	return nil
}
