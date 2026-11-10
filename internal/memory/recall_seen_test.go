package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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

// A corrupt or relative root-<id> file is replaced by the first valid root instead of lingering all session.
func TestRecordSessionRootRepairsCorruptFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwd := canonTemp(t, t.TempDir())
	for _, bad := range []string{"relative/path\n", "\n"} {
		path := sessionRootPath(id)
		write(t, path, bad)
		RecordSessionRoot(id, cwd)
		got, err := os.ReadFile(path)
		if err != nil || !strings.HasPrefix(string(got), "/") {
			t.Fatalf("bad %q not repaired: %q, %v", bad, got, err)
		}
		if SessionRoot(id, "fallback") == "fallback" {
			t.Errorf("SessionRoot still falls back after repairing %q", bad)
		}
	}
}

func TestRecordSessionRootReportsLockTimeout(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwd := canonTemp(t, t.TempDir())
	path := sessionRootPath(id)
	write(t, path+".lock", "")
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() {
		os.Stderr = original
		if err := stderr.Close(); err != nil {
			t.Error(err)
		}
	})
	ran, err := paths.TryFileLock(path+".lock", 0, func() error {
		RecordSessionRoot(id, cwd)
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold session-root lock: ran=%v, err=%v", ran, err)
	}
	output, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := "memory-start: record session root: " + paths.ErrLockTimeout.Error() + ": " + path + ".lock\n"
	if string(output) != want {
		t.Errorf("lock timeout diagnostic: got %q, want %q", output, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("session root must remain unwritten while locked: %v", err)
	}
}

// A valid existing root is kept: the first root wins.
func TestRecordSessionRootKeepsValidFile(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	keep := canonTemp(t, t.TempDir())
	write(t, sessionRootPath(id), keep+"\n")
	RecordSessionRoot(id, canonTemp(t, t.TempDir()))
	if got := SessionRoot(id, ""); got != keep {
		t.Errorf("first root replaced: got %q, want %q", got, keep)
	}
}

// Racing repairs of one corrupt root leave one root, and every caller sees that same root once it returns.
func TestRecordSessionRootRacingRepairsAgree(t *testing.T) {
	isolatedConfig(t)
	const id = "11111111-2222-3333-4444-555555555555"
	cwds := make([]string, 6)
	for i := range cwds {
		cwds[i] = canonTemp(t, t.TempDir())
	}
	for round := range 40 {
		write(t, sessionRootPath(id), "relative/path\n")
		seen := make([]string, len(cwds))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, cwd := range cwds {
			wg.Go(func() {
				<-start
				RecordSessionRoot(id, cwd)
				seen[i] = SessionRoot(id, "")
			})
		}
		close(start)
		wg.Wait()
		final := SessionRoot(id, "")
		for i, got := range seen {
			if got == "" || got != final {
				t.Fatalf("round %d: caller %d saw %q, final root %q", round, i, got, final)
			}
		}
	}
}

// TestSessionRootForSkipsMainProbeOutsideRepo: a cwd git calls no work tree is not probed again for its main checkout.
func TestSessionRootForSkipsMainProbeOutsideRepo(t *testing.T) {
	tempConfig(t)
	dir := canonTemp(t, t.TempDir())
	probes := countGitProbes(t, "--git-common-dir")
	if got := sessionRootFor(dir); got != dir {
		t.Fatalf("sessionRootFor(%s) = %q, want the dir itself", dir, got)
	}
	if *probes != 0 {
		t.Errorf("sessionRootFor ran the main-checkout probe %d times after the toplevel probe failed, want 0", *probes)
	}
}
