package claude

import (
	"context"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/launchplan"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Launch launches Claude Code with alias as the main-loop model ("" = opus) and returns the exit code.
func Launch(alias string, args []string) int {
	as, args, err := providers.TakeAs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	opts, rest, err := launchplan.ParseLaunchArgs(args, true)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if bareHelp(rest) { // claude's own help needs no profile or model, so no lookup may refuse it
		bin, err := ClaudeBinary()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return helptext.WrappedDocs(bin)
	}
	opts.Alias = alias
	plan, err := Claude(providers.WithIdentity(context.Background(), as), opts, rest)
	if err == nil && !opts.PrintEnv && !launchplan.IsHeadless(rest) {
		providers.RecordLastProfile("claude", plan.Set[providers.EnvSessionProfile])
	}
	return launchplan.Run("claude", plan, opts, err)
}

// bareHelp is a lone help flag; `claude mcp --help` still goes to claude so the subcommand's help is kept.
func bareHelp(rest []string) bool { return len(rest) == 1 && helptext.WantsHelp(rest) }

// LaunchScience is `anthropic science`: only a leading --print-env; every other flag is claude-science's own.
func LaunchScience(args []string) int {
	var opts agent.LaunchOptions
	if len(args) > 0 && args[0] == "--print-env" {
		opts.PrintEnv, args = true, args[1:]
	}
	plan, err := ScienceEnv(args)
	return launchplan.Run("claude-science", plan, opts, err)
}

// LaunchAnthropic is first-party Claude Code with a login, OAuth token or API key; vaultSet stores a missing secret.
func LaunchAnthropic(args []string, vaultSet func(name string) int) int {
	if err := databricks.RemovedFlagHint(args); err != nil {
		return helptext.Fail("%v", err)
	}
	o, rest, err := ParseAnthropicArgs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if o.Provider != "" { // a named profile launches like `claude --profile P`, on that profile's credential
		return Launch("", args)
	}
	secret := ""
	if o.Mode != AuthLogin {
		if secret, err = AnthropicSecret(o, vaultSet); err != nil {
			return helptext.Fail("%v", err)
		}
	}
	plan, err := Anthropic(context.Background(), o, rest, secret)
	return launchplan.Run("claude", plan, o.LaunchOptions, err)
}
