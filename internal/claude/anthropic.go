package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// Anthropic auth modes for the `anthropic` verb.
const (
	authLogin  = "login"     // claude.ai login (provider subscription)
	authOAuth  = "oauth"     // CLAUDE_CODE_OAUTH_TOKEN from the vault
	authAPIKey = "x-api-key" // provider anthropic, ANTHROPIC_API_KEY
)

// defaultAPIKeyName is the vault key --x-api-key uses when it carries no =NAME; --oauth uses oauthKey.
const defaultAPIKeyName = proc.EnvAnthropicAPIKey

// anthropicOptions are the launcher flags plus the verb's own auth selection.
type anthropicOptions struct {
	agent.LaunchOptions
	Mode string // authLogin | authOAuth | authAPIKey
	Name string // vault key holding the secret ("" for authLogin)
}

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// parseAnthropicArgs strips --oauth[=NAME] and --x-api-key[=NAME] (a bare flag takes no value), then the shared launcher flags.
func parseAnthropicArgs(args []string) (anthropicOptions, []string, error) {
	o := anthropicOptions{Mode: authLogin}
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
		if err := applyAnthropicAuthArg(&o, name, val, hasVal); err != nil {
			return o, nil, err
		}
	}
	base, rest, err := agent.ParseArgs(kept, true)
	if err != nil {
		return o, nil, err
	}
	if base.Provider != "" && o.Mode != authLogin {
		return o, nil, errors.New("--profile picks the profile's own credential; drop --oauth/--x-api-key")
	}
	o.LaunchOptions = base
	return o, rest, nil
}

func applyAnthropicAuthArg(o *anthropicOptions, name, val string, hasVal bool) error {
	mode, def := authOAuth, oauthKey
	if name == "--x-api-key" {
		mode, def = authAPIKey, defaultAPIKeyName
	}
	if o.Mode == mode {
		return fmt.Errorf("%s given more than once; pass it a single time", name)
	}
	if o.Mode != authLogin {
		return errors.New("--oauth and --x-api-key cannot be combined; pick one auth mode")
	}
	if hasVal {
		if err := checkKeyName(name, val); err != nil {
			return err
		}
		def = val
	}
	o.Mode, o.Name = mode, def
	return nil
}

// checkKeyName accepts only an env-var-shaped vault key name; a credential-shaped value is refused unechoed.
func checkKeyName(flag, v string) error {
	if textutil.LooksSecret(v) || !envNameRE.MatchString(v) {
		return fmt.Errorf("%s=NAME takes a vault key name ([A-Z_][A-Z0-9_]*); store secrets with claude-code vault set", flag)
	}
	return nil
}

// anthropicPlan resolves a first-party Claude Code launch; secret is the value for the oauth and x-api-key modes.
func anthropicPlan(ctx context.Context, o anthropicOptions, args []string, secret string) (agent.Plan, error) {
	opts := o.LaunchOptions
	reg, err := providers.Load()
	if err != nil {
		return agent.Plan{}, err
	}
	method := providers.AuthCLI
	if o.Mode == authAPIKey {
		method = providers.AuthKey
	}
	if opts.Provider = anthropicProfile(reg, method); opts.Provider == "" {
		return agent.Plan{}, fmt.Errorf("no anthropic profile with %s auth in the registry", method)
	}
	plan, err := claudePlan(ctx, opts, args)
	if err != nil || plan.ForeignBase { // a foreign shell endpoint never receives the vault secret
		return plan, err
	}
	applyAnthropicSecret(&plan, o, secret)
	return plan, nil
}

func applyAnthropicSecret(plan *agent.Plan, o anthropicOptions, secret string) {
	switch o.Mode {
	case authOAuth:
		plan.Set[oauthKey] = secret
	case authAPIKey:
		// The apiKeyHelper reads the registry's env name, so carry this key directly.
		if o.Name != defaultAPIKeyName {
			plan.Set[defaultAPIKeyName] = secret
			delete(plan.Set, "CLAUDE_CODE_API_KEY_HELPER_TTL_MS")
			if len(plan.Argv) >= 2 && plan.Argv[0] == "--settings" {
				plan.Argv = plan.Argv[2:]
			}
			plan.Files = nil
		}
	}
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

// anthropicSecret reads the mode's vault key; when it is missing and a terminal is attached, offers to store it.
func anthropicSecret(o anthropicOptions, store func(name string) int) (string, error) {
	if v, err := readAnthropicSecret(o); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	missing := fmt.Errorf("%s is not in the vault; store it with `claude-code vault set %s`", o.Name, o.Name)
	if o.PrintEnv || !tui.StdinIsTerminal() || !proc.IsTerminal(os.Stderr) {
		return "", missing
	}
	if !tui.Confirm(o.Name+" is not in the vault — set it now?", true) {
		return "", missing
	}
	if err := storeAnthropicSecret(o, store); err != nil {
		return "", err
	}
	if v, err := readAnthropicSecret(o); err != nil {
		return "", fmt.Errorf("%s: %w", o.Name, err)
	} else if v != "" {
		return v, nil
	}
	return "", missing
}

func readAnthropicSecret(o anthropicOptions) (string, error) {
	if o.Mode == authOAuth {
		return providers.StoredSecret(o.Name), nil
	}
	v, _, err := vault.EffectiveSecret(o.Name)
	return v, err
}

func storeAnthropicSecret(o anthropicOptions, store func(name string) int) error {
	if o.Mode == authOAuth && tui.Confirm("Run `claude setup-token` first to mint one?", true) {
		runSetupToken()
	}
	if code := store(o.Name); code != 0 {
		return fmt.Errorf("%s was not stored", o.Name)
	}
	return nil
}

// runSetupToken runs `claude setup-token` on the inherited terminal; a failure only means the user pastes a token.
func runSetupToken() {
	bin, err := ClaudeBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code:", err)
		return
	}
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, bin, "setup-token")
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(ctx) // drops the vault key and routing vars; HOME, TERM, DISPLAY stay
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: claude setup-token:", err)
	}
}
