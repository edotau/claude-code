package network

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agent.StubProxyForTest("http://127.0.0.1:9999", "rtr-secret-test")
	agentconfig.SetCatalogWarn(io.Discard)
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	gateway.HolderComm = func(int) string { return "claude-code" }
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

func TestNetworkBareAndHelpPrintUsageExitZero(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
		if code != 0 {
			t.Errorf("network %v: exit %d, want 0", args, code)
		}
		for _, sub := range []string{"tailscale", "browser", "websearch", "remote"} {
			if !strings.Contains(stderr, sub) {
				t.Errorf("network %v: help lacks %q: %q", args, sub, stderr)
			}
		}
	}
}

func TestNetworkUnknownSubverb(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Run([]string{"bogus"}) })
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) || !strings.Contains(stderr,
		"tailscale|browser|websearch|remote") {
		t.Errorf("stderr = %q", stderr)
	}
}
