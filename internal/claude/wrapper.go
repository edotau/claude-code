package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// launcherEnv holds the PID that ran bin/claude; exec keeps the PID, so seeing our own means the launcher looped.
const launcherEnv = "HARNESS_CLAUDE_LAUNCHER"

// nativeSubcommands manage the install or its config and call no model, so they skip the overlay.
var nativeSubcommands = []string{"--version", "-v", "update", "upgrade", "install", "doctor", "auth", "setup-token",
	"mcp", "plugin", "plugins"}

// LaunchWrapped is bin/claude: the session-profile launch, or native claude with a stderr note if planning fails.
func LaunchWrapped(args []string) int {
	native, args := takeNative(args)
	if os.Getenv(launcherEnv) == strconv.Itoa(os.Getpid()) {
		if native == "" {
			native = newestInstalledClaude()
		}
		return execNative(native, args, errors.New("bin/claude re-entered itself"))
	}
	if len(args) > 0 && slices.Contains(nativeSubcommands, args[0]) {
		return execNative(native, args, nil)
	}
	as, rest, err := providers.TakeAs(args)
	var opts agent.LaunchOptions
	if err == nil {
		opts, rest, err = agent.ParseLaunchArgs(rest, true)
	}
	var plan agent.Plan
	if err == nil {
		opts.Binary = native
		plan, err = claudePlan(providers.WithIdentity(context.Background(), as), opts, rest)
	}
	if err != nil {
		return execNative(native, args, err)
	}
	plan.Set[launcherEnv] = strconv.Itoa(os.Getpid())
	return agent.RunPlan("claude", plan, opts, nil)
}

// takeNative strips a leading absolute path to a claude executable: the VS Code wrapper's bundled binary.
func takeNative(args []string) (string, []string) {
	if len(args) == 0 || !filepath.IsAbs(args[0]) || strings.TrimSuffix(filepath.Base(args[0]), ".exe") != "claude" {
		return "", args
	}
	if fi, err := os.Stat(args[0]); err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		return "", args
	}
	return args[0], args[1:]
}

// execNative runs native claude; cause, when set, is why the overlay is skipped, and the autoMode layer still rides.
func execNative(native string, args []string, cause error) int {
	if native == "" {
		var err error
		if os.Getenv(launcherEnv) == strconv.Itoa(os.Getpid()) {
			err = errors.New("claude: launcher loop and no native install in ~/.local/share/claude/versions")
		} else {
			native, err = paths.NativeClaude()
		}
		if err != nil {
			return helptext.Fail("%v", err)
		}
	}
	if cause != nil {
		fmt.Fprintf(os.Stderr, "claude: harness launch failed (%v); running native claude\n", cause)
		args = withAutoMode(args)
	}
	env := append(os.Environ(), launcherEnv+"="+strconv.Itoa(os.Getpid()))
	return helptext.Fail("exec %s: %v", native, proc.Exec(native, args, env))
}

// withAutoMode prepends the embedded --settings layer unless the caller passed its own.
func withAutoMode(args []string) []string {
	if slices.ContainsFunc(args, func(a string) bool { return a == "--settings" || strings.HasPrefix(a, "--settings=") }) {
		return args
	}
	doc, err := settings.LaunchSettings(false)
	if err != nil {
		return args
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return args
	}
	return append([]string{"--settings", string(raw)}, args...)
}
