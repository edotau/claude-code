package router

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// TestDaemonCommandArgs: the detached daemon re-enters the binary as `gateway proxy --serve`, not the removed `router serve`.
func TestDaemonCommandArgs(t *testing.T) {
	cmd := daemonCommand("/bin/claude-code")
	if want := []string{"/bin/claude-code", "gateway", "proxy", "--serve"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("daemon args = %v, want %v", cmd.Args, want)
	}
}

// A test binary must never bind the live port: Serve's reclaim would SIGTERM the user's running router.
func TestServeRefusesDefaultPortUnderTest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the guard Serve stops at ctx.Err, so a regression fails here instead of binding the port
	if err := Serve(ctx, DefaultPort); !errors.Is(err, errTestBinary) {
		t.Fatalf("Serve(DefaultPort) under test = %v, want errTestBinary", err)
	}
}
