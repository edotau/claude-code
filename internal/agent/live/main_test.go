package live

import (
	"context"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// testLauncher stands in for launch.Launcher (agent cannot import launch): the found claude with the caller's args.
type testLauncher struct{}

func (testLauncher) Claude(_ context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	bin, err := claude.ClaudeBinary()
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	return agent.ClaudeLaunch{Binary: bin, Argv: args, Env: os.Environ(), Set: map[string]string{providers.EnvSessionProfile: o.Provider}}, nil
}
