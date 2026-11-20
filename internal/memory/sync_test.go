package memory

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func syncFixture(t *testing.T, on string, peers ...string) *Spawns {
	t.Helper()
	tempConfig(t)
	t.Setenv("NETWORK_MEMORY", on)
	t.Setenv("CLAUDE_REMOTE_HOST", "")
	prev := remoteHosts
	remoteHosts = func() []string { return peers }
	t.Cleanup(func() { remoteHosts = prev })
	return StubSpawn(t, true)
}

func syncStamp() string { return filepath.Join(paths.StateDir(), "preflight", syncStampName) }

func TestSyncOffByDefault(t *testing.T) {
	spawns := syncFixture(t, "", "peer.example")
	remoteHosts = func() []string { return []string{"peer.example"} } // a peer list alone is not an opt-in
	now := time.Now()
	for _, setup := range []func(){func() {}, func() { t.Setenv("CLAUDE_REMOTE_HOST", "peer.example") }, func() { t.Setenv("NETWORK_MEMORY", "off") }} {
		setup()
		if SyncIfDue(now, now, t.TempDir()) {
			t.Fatal("SyncIfDue started a pass without the opt-in")
		}
	}
	if spawns.Sync.Load() != 0 || pathExists(syncStamp()) || pathExists(syncStatePath()) {
		t.Fatalf("off left traces: spawns=%d", spawns.Sync.Load())
	}
	t.Setenv("NETWORK_MEMORY", "ON")
	if !SyncOn() {
		t.Fatal("NETWORK_MEMORY=ON is not an opt-in")
	}
}

func TestSyncDecide(t *testing.T) {
	cases := []struct {
		seen, local, peer string
		want              SyncAction
	}{
		{"a b", "a", "c", SyncTakePeer},
		{"a b", "c", "b", SyncSendLocal},
		{"a b", "c", "d", SyncDiffer},
		{"", "c", "d", SyncDiffer},
		{"a b", "a", "b", SyncStanding},
		{"a", "a", "a", SyncEqual},
	}
	for _, c := range cases {
		if got := DecideSync(c.seen, c.local, c.peer); got != c.want {
			t.Errorf("DecideSync(%q,%q,%q) = %d, want %d", c.seen, c.local, c.peer, got, c.want)
		}
	}
}

func TestSyncIfDue(t *testing.T) {
	spawns := syncFixture(t, "on", "peer.example")
	now := time.Now()
	write(t, syncStamp(), "")
	stampAt := func(at time.Time) {
		if err := os.Chtimes(syncStamp(), at, at); err != nil {
			t.Fatal(err)
		}
	}
	stampAt(now.Add(-time.Hour))
	if SyncIfDue(now, now, "") {
		t.Fatal("a fresh stamp started a pass")
	}
	stampAt(now.Add(-5 * time.Hour))
	if SyncIfDue(now, now.Add(-6*time.Hour), "") {
		t.Fatal("a due slot without activity started a pass")
	}
	if !SyncIfDue(now, now, "") || SyncIfDue(now, now, "") || spawns.Sync.Load() != 1 {
		t.Fatalf("due and active: want exactly one pass, got %d", spawns.Sync.Load())
	}
	spawns = syncFixture(t, "on")
	if SyncIfDue(now, now, "") || spawns.Sync.Load() != 0 {
		t.Fatal("an empty peer list started a pass")
	}
}

func TestSyncStateAndChangedSince(t *testing.T) {
	tempConfig(t)
	st := LoadSync()
	if len(st.Peers) != 0 {
		t.Fatal("fresh state is not empty")
	}
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	st.Peers["peer.example"] = &PeerSync{Pulled: "h 0", Pull: SyncDir{OK: at, Differ: 2}, Seen: map[string]string{"a.md": "x"}}
	if err := SaveSync(st); err != nil {
		t.Fatal(err)
	}
	got := LoadSync().Peers["peer.example"]
	if got == nil || got.Pulled != "h 0" || !got.Pull.OK.Equal(at) || got.Pull.Differ != 2 || got.Seen["a.md"] != "x" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	root := paths.MemoryStoreDir()
	write(t, filepath.Join(root, layoutFile), layoutFlat+"\n")
	write(t, filepath.Join(root, "notes", "a.md"), "one")
	write(t, filepath.Join(root, "notes", "b.md"), "two")
	write(t, filepath.Join(root, "MEMORY.md"), "generated")
	write(t, filepath.Join(root, ".git", "x"), "git")
	local := HashStore()
	if len(local) != 2 || local["notes/a.md"] == "" {
		t.Fatalf("HashStore = %v, want only the two notes", local)
	}
	seen := map[string]string{"notes/a.md": local["notes/a.md"] + " peerhash"}
	if got := ChangedSince(local, seen); !slices.Equal(got, []string{"notes/b.md"}) {
		t.Fatalf("ChangedSince = %v, want [notes/b.md]", got)
	}
}

func TestSyncLocalTokenTracksContent(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	write(t, Path("topic.md"), "one\n")
	before := LocalToken()
	write(t, Path("topic.md"), "two\n") // the same dirty-line count, other bytes
	if after := LocalToken(); after == before || after == "" {
		t.Fatalf("LocalToken did not move with the content: %q -> %q", before, after)
	}
}
