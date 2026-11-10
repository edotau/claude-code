package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Session-root files age out with their session, but survive while the session's count- file is fresh.
func TestPruneSeenSessionRoots(t *testing.T) {
	isolatedConfig(t)
	state := StateDir()
	old := time.Now().Add(-2 * seenMaxAge)
	const stale, live = "stale-session", "live-session"
	for _, name := range []string{"root-" + stale, "root-" + stale + ".lock", "root-" + live, "root-" + live + ".lock", "count-" + stale, "count-" + live} {
		write(t, filepath.Join(state, name), "x")
		if name != "count-"+live {
			if err := os.Chtimes(filepath.Join(state, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	pruneSeen(filepath.Join(state, "recall-seen"))
	for name, wantGone := range map[string]bool{
		"root-" + stale: true, "root-" + stale + ".lock": true, "count-" + stale: true,
		"root-" + live: false, "root-" + live + ".lock": false,
	} {
		_, err := os.Stat(filepath.Join(state, name))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s gone=%v, want %v", name, gone, wantGone)
		}
	}
}
