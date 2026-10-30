package agent

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/agent/vendors"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	NoticeOut = io.Discard
	vendors.SetCatalogWarn(io.Discard)
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
