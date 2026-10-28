package providers

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// PresetDatabricks derives every route, the auth commands and the launch env from one host + one CLI profile.
const PresetDatabricks = "databricks"

var (
	cliProfileRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	envKeyRE     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	secretNameRE = regexp.MustCompile(`(^|_)(KEY|TOKEN|SECRET|PASSWORD|HEADERS|AUTH|CREDENTIALS?|PAT|BEARER)(_|$)`)
)

// DatabricksProfiles lists the profiles under the databricks provider in declared order.
func DatabricksProfiles(r *Registry) []string { return r.ProfilesOf(GroupDatabricks) }

// BetaOffKey is the launch env key that disables Claude Code's experimental betas.
const BetaOffKey = "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS"

// databricksModels are the canonical ids a databricks preset seeds into empty slots.
var databricksModels = map[string]string{
	"opus": "claude-opus-5-5", "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5",
}

// expandPresets fills each preset provider's empty fields; load-only. Invalid presets are left for Validate.
func expandPresets(r *Registry) {
	for _, p := range r.Providers {
		if p == nil || p.Preset != PresetDatabricks || presetHost(p.Host) == "" {
			continue
		}
		expandDatabricks(p)
	}
}

// decorateModels rewrites every provider's slot ids to wire ids in memory; load-only, never persisted.
func decorateModels(r *Registry) {
	for _, p := range r.Providers {
		if p == nil || len(p.Models) == 0 || p.SpellingRule() == "" {
			continue
		}
		out := make(map[string]string, len(p.Models))
		for slot, id := range p.Models {
			out[slot] = p.WireModel(id)
		}
		p.Models = out
	}
}

// presetHost returns the https://host of an absolute https host URL with no path, else "".
func presetHost(host string) string {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return ""
	}
	return "https://" + u.Host
}

func expandDatabricks(p *Provider) {
	host := presetHost(p.Host)
	if p.Kind == "" {
		p.Kind = DialectAnthropic
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
	for slot, id := range databricksModels {
		if p.Models[slot] == "" {
			p.Models = withKey(p.Models, slot, id)
		}
	}
	if p.Env[BetaOffKey] == "" {
		p.Env = withKey(p.Env, BetaOffKey, "1")
	}
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
	return mint + " -o json | jq -r .access_token", mint + " --force-refresh -o json | jq -r .access_token"
}

// ValidID reports whether s is a legal provider row id / cliProfile name.
func ValidID(s string) bool { return cliProfileRE.MatchString(s) }

func expandServing(p *Provider, host string) {
	if p.BaseURL == "" {
		p.BaseURL = host + "/serving-endpoints/anthropic"
	}
	for _, d := range []string{DialectOpenAI, DialectResponses} {
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
	if p.Routes[DialectResponses] == "" {
		p.Routes = withKey(p.Routes, DialectResponses, host+"/ai-gateway/codex/v1")
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
	if presetHost(p.Host) == "" {
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

// harnessEnvRE and harnessEnvKeys mirror launch.wipeKeys (launch imports providers, so no import); the harness owns them.
var (
	harnessEnvRE   = regexp.MustCompile(`^(ANTHROPIC_|HARNESS_|CLAUDE_PROFILE$|CLAUDE_(MODEL|OPUS_MODEL|SONNET_MODEL|HAIKU_MODEL|FABLE_MODEL)$)`)
	harnessEnvKeys = []string{
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS", "CLAUDE_CODE_SUBAGENT_MODEL", "MAX_THINKING_TOKENS",
		"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR",
	}
)

// validateEnv checks the launch env keys: well-formed names, and none credential-shaped (secrets belong in auth).
func validateEnv(n string, env map[string]string) []string {
	var errs []string
	for k := range env {
		switch {
		case !envKeyRE.MatchString(k):
			errs = append(errs, fmt.Sprintf("%s: env key %q must match [A-Z_][A-Z0-9_]*", n, k))
		case secretNameRE.MatchString(k):
			errs = append(errs, fmt.Sprintf("%s: env key %q looks like a credential; secrets belong in auth", n, k))
		case harnessEnvRE.MatchString(k) || slices.Contains(harnessEnvKeys, k):
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
	if !jwtClaims(v, &claims) || claims.Iss == "" {
		return "", false
	}
	u, err := url.Parse(claims.Iss)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Host, true
}

// checkIssuer fails a preset provider's minted JWT whose issuer host is not the preset host; never names the token.
func checkIssuer(p *Provider, v string) error {
	want := strings.TrimPrefix(presetHost(p.Host), "https://")
	if p.Preset == "" || want == "" {
		return nil
	}
	if iss, ok := jwtIssuerHost(v); ok && iss != want {
		return fmt.Errorf("%s: token issuer host %s does not match preset host %s (wrong cliProfile?)", p.Name, iss, want)
	}
	return nil
}
