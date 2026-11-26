package dispatchhook

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory/memtest"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
)

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	tb := &hosttest.MainTB{}
	memtest.StubSpawn(tb, true)
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
