package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

func cmdClaude(args []string) int { return launchClaude("", args) }

// launchClaude launches Claude Code with alias as the main-loop model ("" = opus).
func launchClaude(alias string, args []string) int {
	as, args, err := providers.TakeAs(args)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	opts, rest, err := parseLaunchArgs(args, true)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	opts.Alias = alias
	plan, err := claude.Claude(providers.WithIdentity(context.Background(), as), opts, rest)
	if err == nil && !opts.PrintEnv && !agent.IsHeadless(rest) {
		providers.RecordLastProfile("claude", plan.Set[providers.EnvSessionProfile])
	}
	return runPlan("claude", plan, opts, err)
}

// aliasVerb is `claude-code <alias>`: Claude Code on the session profile with that alias as the main model.
func aliasVerb(alias string) func([]string) int {
	return func(a []string) int { return launchClaude(alias, a) }
}

// parseLaunchArgs refuses a removed flag (with its hint) before agent.ParseArgs strips the harness flags.
func parseLaunchArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	if err := databricks.RemovedFlagHint(args); err != nil {
		return agent.LaunchOptions{}, nil, err
	}
	return agent.ParseArgs(args, claude)
}

func launchAgent(name string, args []string) int {
	opts, rest, err := parseLaunchArgs(args, false)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	plan, err := agent.LaunchPlan(context.Background(), name, opts, rest)
	if err == nil {
		warnVersion(name, plan.Binary, opts, rest)
		if !opts.PrintEnv && !agent.IsHeadless(rest) {
			providers.RecordLastProfile(name, plan.Profile)
		}
	}
	return runPlan(name, plan, opts, err)
}

// warnVersion prints the one-line out-of-bounds warning for an interactive launch of the agent binary at path.
func warnVersion(name, path string, opts agent.LaunchOptions, args []string) {
	if !agent.WantsVersionWarning(opts, args) || !agentVersionTTY() {
		return
	}
	if line := tools.VersionWarning(name, path); line != "" {
		fmt.Fprintln(versionWarnOut, line)
	}
}

// agentVersionTTY and versionWarnOut are the version warning's terminal gate and sink (tests swap them).
var (
	agentVersionTTY           = func() bool { return ansi.IsTerminal(os.Stderr) }
	versionWarnOut  io.Writer = os.Stderr
)

func cmdUsage(args []string) int { return usagecmd.Run(args, paths.ConfigDir(), os.Stdout, os.Stderr) }

// cmdScience takes only a leading --print-env; every other flag is claude-science's own.
func cmdScience(args []string) int {
	var opts agent.LaunchOptions
	if len(args) > 0 && args[0] == "--print-env" {
		opts.PrintEnv, args = true, args[1:]
	}
	plan, err := claude.ScienceEnv(args)
	return runPlan("claude-science", plan, opts, err)
}

func runPlan(name string, plan agent.Plan, opts agent.LaunchOptions, err error) int {
	plan.Agent = name
	if usage, ok := errors.AsType[*agent.UsageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return helptext.Fail("%v", err)
	}
	ov, oerr := launchOverlay(opts)
	if oerr == nil {
		oerr = plan.CheckOverlay(ov) // a launch-owned route name is refused, not silently overridden
	}
	if oerr != nil {
		if plan.OnExecFail != nil {
			plan.OnExecFail() // a minted session token dies with the refused launch
		}
		fmt.Fprintln(os.Stderr, "claude-code:", oerr)
		return 2
	}
	plan = plan.WithOverlay(ov)
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		if agentconfig.OverlayDebugOn() {
			plan.RecordEnv() // dry exec: the env Exec would hand the child
		}
		return 0
	}
	return helptext.Fail("exec %s: %v", plan.Binary, plan.Exec())
}

// launchOverlay is the overlay for an interactive launch: --overlay, else $COWORKER_OVERLAY; --print-env keeps vault
// refs unresolved so it shows their names, never their values.
func launchOverlay(opts agent.LaunchOptions) (agentconfig.Resolved, error) {
	o, err := agentconfig.LoadOverlay(opts.Overlay)
	if err != nil {
		return agentconfig.Resolved{}, err
	}
	if opts.PrintEnv {
		return agentconfig.Resolved{Set: o.Set, Unset: o.Unset, Keep: o.Keep}, nil
	}
	return agentconfig.ResolveOverlay(o)
}

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
	Hint:  settings.HookModules() + "\nevents: <Event> runs the dispatcher, --<module> one module, --all every module",
}

func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}
