package launchcli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// IsTerminal is the TTY check internal/cli wires at startup; the zero value says no terminal.
var IsTerminal = func(*os.File) bool { return false }

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

// Positional guards a dash-led prompt from flag parsing.
func Positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}

// ParseLaunchArgs refuses a removed flag (with its hint) before ParseArgs strips the harness flags.
func ParseLaunchArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	if err := databricks.RemovedFlagHint(args); err != nil {
		return agent.LaunchOptions{}, nil, err
	}
	return ParseArgs(args, claude)
}

// Run applies the launch overlay to plan, then prints it (--print-env) or execs it; a *UsageError exits 2.
func Run(name string, plan agent.Plan, opts agent.LaunchOptions, err error) int {
	plan.Agent = name
	if usage, ok := errors.AsType[*UsageError](err); ok {
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

// launchOverlay is --overlay, else $COWORKER_OVERLAY; --print-env keeps vault refs unresolved (names, never values).
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
