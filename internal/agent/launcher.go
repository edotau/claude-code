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

// Launcher resolves a Claude launch; claudeLauncher is the default and tests fake it.
type Launcher interface {
	Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error)
}

// claudeLauncher implements Launcher over Claude: the plan's overlay is written, its env layered on the process env.
type claudeLauncher struct{}

func (claudeLauncher) Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error) {
	plan, err := Claude(ctx, LaunchOptions{Provider: o.Provider, Model: o.Model, KeepEnv: o.KeepEnv}, args)
	if err != nil {
		return ClaudeLaunch{}, err
	}
	if err := plan.WriteOverlay(); err != nil {
		return ClaudeLaunch{}, err
	}
	return ClaudeLaunch{Binary: plan.Binary, Argv: plan.Argv, Env: plan.Env(os.Environ()), Set: plan.Set}, nil
}
