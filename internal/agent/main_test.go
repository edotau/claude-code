package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	NoticeOut = io.Discard
	sharedPoll = 25 * time.Millisecond
	SetCatalogWarn(io.Discard)
	mintSession = func(context.Context, string, int) (string, error) { return "", errors.New("session mint is stubbed") }
	revokeSession = func(context.Context, string) error { return nil }
	copilotGOOS = "linux" // registry tests run on the macOS runner too; the darwin refusal test sets darwin itself
	ensureRouter = func(context.Context) (string, error) { return testRouterBase, nil }
	routerSecret = func() (string, error) { return testRouterSecret, nil }
	os.Exit(hosttest.Main(m))
}

// quietStderr sends os.Stderr to /dev/null for the test; production notices read os.Stderr at call time.
func quietStderr(t *testing.T) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = null
	t.Cleanup(func() { os.Stderr = old; null.Close() })
}

// testLauncher stands in for launch.Launcher (agent cannot import launch): the found claude with the caller's args.
type testLauncher struct{}

func (testLauncher) Claude(_ context.Context, o ClaudeOptions, args []string) (ClaudeLaunch, error) {
	bin, err := ClaudeBinary()
	if err != nil {
		return ClaudeLaunch{}, err
	}
	return ClaudeLaunch{Binary: bin, Argv: args, Env: os.Environ(), Set: map[string]string{providers.PinProfile: o.Provider}}, nil
}

// activateAgents turns the external agents on (an external agent with no profile is off) via registry profiles.
func activateAgents(t *testing.T, agents ...string) {
	t.Helper()
	profiles := map[string]string{"codex": "chatgpt", "gemini": "antigravity", "copilot": "personal", "opencode": "nvidia"}
	if err := UpdateAgentRecords(func(m map[string]AgentRecord) {
		for _, a := range agents {
			r := m[a]
			r.Profile = profiles[a]
			m[a] = r
		}
	}); err != nil {
		t.Fatal(err)
	}
}
