package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestClaimRestartRejectsUnrecordableClaim(t *testing.T) {
	for _, failure := range []string{"lock-and-stamp", "stamp-only"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			path := filepath.Join(paths.StateDir(), "proxy.restart-claim")
			if failure == "lock-and-stamp" {
				writeProxyFixture(t, filepath.Dir(paths.StateDir()), "not a directory")
			} else {
				writeProxyFixture(t, filepath.Join(path, "blocks-stamp"), "occupied")
				old := time.Now().Add(-2 * restartRetry)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if claimRestart(time.Now()) {
					t.Fatal("restart claimed without a writable throttle stamp")
				}
			}
		})
	}
}

// TestRestartReportIfChanged: --if-changed leaves alone only a proxy recorded as running the installed binary.
func TestRestartReportIfChanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prevComm, prevHash := HolderComm, selfHash
	t.Cleanup(func() { HolderComm, selfHash = prevComm, prevHash })
	HolderComm = func(int) string { return "claude-code" }
	selfHash = func() string { return "installed" }
	port := occupy(t, func(http.ResponseWriter, *http.Request) {})
	for _, tc := range []struct{ name, exe, want string }{
		{"same", "installed", "unchanged"}, {"different", "older", "would restart"}, {"unknown", "", "would restart"},
	} {
		cleanup, err := WriteState(State{PID: os.Getpid(), Port: port, Exe: tc.exe})
		if err != nil {
			t.Fatal(err)
		}
		if got := RestartReport(context.Background(), true, false, true).Status; got != tc.want {
			t.Errorf("%s: --if-changed status = %q, want %q", tc.name, got, tc.want)
		}
		if got := RestartReport(context.Background(), true, false, false).Status; got != "would restart" {
			t.Errorf("%s: plain restart status = %q, want would restart", tc.name, got)
		}
		cleanup()
	}
}
