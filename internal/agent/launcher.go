package agent

import (
	"context"
	"io"
	"os"
)

// NoticeOut is where launch and vendor-fallback notices print; tests discard it (also via other packages' TestMain).
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

// Launcher resolves a Claude launch; the caller (cli) supplies the launch package's implementation.
type Launcher interface {
	Claude(ctx context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error)
}
