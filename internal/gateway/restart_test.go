package gateway

import (
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
