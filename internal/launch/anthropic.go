package launch

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// Anthropic auth modes for the `anthropic` verb.
const (
	AuthLogin  = "login"     // claude.ai login (provider subscription)
	AuthOAuth  = "oauth"     // CLAUDE_CODE_OAUTH_TOKEN from the vault
	AuthAPIKey = "x-api-key" // provider anthropic, ANTHROPIC_API_KEY
)

// DefaultOAuthName and DefaultAPIKeyName are the vault keys used when a flag carries no =NAME.
const (
	DefaultOAuthName  = oauthKey
	DefaultAPIKeyName = "ANTHROPIC_API_KEY"
)

// AnthropicOptions are the launcher flags plus the verb's own auth selection.
type AnthropicOptions struct {
	Options
	Mode string // AuthLogin | AuthOAuth | AuthAPIKey
	Name string // vault key holding the secret ("" for AuthLogin)
}

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ParseAnthropicArgs strips --oauth[=NAME] and --x-api-key[=NAME] (a bare flag takes no value), then the shared launcher flags.
func ParseAnthropicArgs(args []string) (AnthropicOptions, []string, error) {
	o := AnthropicOptions{Mode: AuthLogin}
	var kept []string
	for i, a := range args {
		if a == "--" {
			kept = append(kept, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(a, "=")
		if name != "--oauth" && name != "--x-api-key" {
			kept = append(kept, a)
			continue
		}
		mode, def := AuthOAuth, DefaultOAuthName
		if name == "--x-api-key" {
			mode, def = AuthAPIKey, DefaultAPIKeyName
		}
		if o.Mode == mode {
			return o, nil, fmt.Errorf("%s given more than once; pass it a single time", name)
		}
		if o.Mode != AuthLogin {
			return o, nil, fmt.Errorf("--oauth and --x-api-key cannot be combined; pick one auth mode")
		}
		if hasVal {
			if err := checkKeyName(name, val); err != nil {
				return o, nil, err
			}
			def = val
		}
		o.Mode, o.Name = mode, def
	}
	base, rest, err := ParseArgs(kept, true)
	if err != nil {
		return o, nil, err
	}
	if base.Provider != "" {
		return o, nil, fmt.Errorf("anthropic picks its own profile; drop --profile (or use claude-code opus --profile ...)")
	}
	o.Options = base
	return o, rest, nil
}

// checkKeyName accepts only an env-var-shaped vault key name; a credential-shaped value is refused unechoed.
func checkKeyName(flag, v string) error {
	if shapes.Looks(v) {
		return fmt.Errorf("%s=NAME takes a vault key name ([A-Z_][A-Z0-9_]*); store secrets with claude-code vault set", flag)
	}
	if !envNameRE.MatchString(v) {
		return fmt.Errorf("%s=NAME takes a vault key name ([A-Z_][A-Z0-9_]*); store secrets with claude-code vault set", flag)
	}
	return nil
}

// Anthropic resolves a first-party Claude Code launch; secret is the value for the oauth and x-api-key modes.
func Anthropic(ctx context.Context, o AnthropicOptions, args []string, secret string) (Plan, error) {
	opts := o.Options
	reg, err := providers.Load()
	if err != nil {
		return Plan{}, err
	}
	method := providers.AuthCLI
	if o.Mode == AuthAPIKey {
		method = providers.AuthKey
	}
	if opts.Provider = anthropicProfile(reg, method); opts.Provider == "" {
		return Plan{}, fmt.Errorf("no anthropic profile with %s auth in the registry", method)
	}
	plan, err := Claude(ctx, opts, args)
	if err != nil {
		return plan, err
	}
	switch o.Mode {
	case AuthOAuth:
		plan.Set[oauthKey] = secret
	case AuthAPIKey:
		if o.Name != DefaultAPIKeyName { // the apiKeyHelper reads the registry's env name, so carry this key directly
			plan.Set[DefaultAPIKeyName] = secret
			delete(plan.Set, "CLAUDE_CODE_API_KEY_HELPER_TTL_MS")
			if len(plan.Argv) >= 2 && plan.Argv[0] == "--settings" {
				plan.Argv = plan.Argv[2:]
			}
			plan.Overlay, plan.OverlayPath = nil, ""
		}
	}
	return plan, nil
}

// anthropicProfile is the first anthropic-provider profile signing in by method ("" when none).
func anthropicProfile(reg *providers.Registry, method string) string {
	for _, n := range reg.ProfilesOf("anthropic") {
		if reg.Providers[n].Auth.Method() == method {
			return n
		}
	}
	return ""
}
