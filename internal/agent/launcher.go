package agent

import (
	"context"
	"io"
	"os"
)

// NoticeOut is where launch and agent-fallback notices print; tests discard it (also via other packages' TestMain).
var NoticeOut io.Writer = os.Stderr

// ClaudeLaunch is a resolved Claude Code launch: the binary, its argv and env, and the route keys the plan set.
type ClaudeLaunch struct {
	Binary    string
	Argv, Env []string
	Set       map[string]string
}

// ClaudeOptions picks the session route for a Claude launch.
type ClaudeOptions struct {
	Provider, Model string
	KeepEnv         []string
}

// Launcher resolves a Claude launch; the claude package registers the default and tests fake it.
type Launcher interface {
	Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error)
}

var defaultLauncher Launcher

// RegisterLauncher sets the Launcher a Client and the bridge use by default; the claude package registers its own.
func RegisterLauncher(l Launcher) { defaultLauncher = l }

// DefaultLauncher is the registered default Launcher.
func DefaultLauncher() Launcher { return defaultLauncher }
