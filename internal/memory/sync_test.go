package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		want              syncAction
	}{
		{"a b", "a", "c", syncTakePeer},
		{"a b", "c", "b", syncSendLocal},
		{"a b", "c", "d", syncDiffer},
		{"", "c", "d", syncDiffer},
		{"a b", "a", "b", syncStanding},
		{"a", "a", "a", syncEqual},
	}
	for _, c := range cases {
		if got := decideSync(c.seen, c.local, c.peer); got != c.want {
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
	stampAt(now.Add(-syncEvery / 2))
	if SyncIfDue(now, now, "") {
		t.Fatal("a fresh stamp started a pass")
	}
	stampAt(now.Add(-syncEvery - time.Hour))
	if SyncIfDue(now, now.Add(-syncEvery-2*time.Hour), "") {
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
	st := loadSync()
	if len(st.Peers) != 0 {
		t.Fatal("fresh state is not empty")
	}
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	st.Peers["peer.example"] = &PeerSync{Pulled: "h 0", Pull: SyncDir{OK: at, Differ: 2}, Seen: map[string]string{"a.md": "x"}}
	if err := SaveSync(st); err != nil {
		t.Fatal(err)
	}
	got := loadSync().Peers["peer.example"]
	if got == nil || got.Pulled != "h 0" || !got.Pull.OK.Equal(at) || got.Pull.Differ != 2 || got.Seen["a.md"] != "x" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	root := paths.MemoryStoreDir()
	write(t, filepath.Join(root, layoutFile), layoutFlat+"\n")
	write(t, filepath.Join(root, "notes", "a.md"), "one")
	write(t, filepath.Join(root, "notes", "b.md"), "two")
	write(t, filepath.Join(root, "MEMORY.md"), "generated")
	write(t, filepath.Join(root, ".git", "x"), "git")
	local := hashStore()
	if len(local) != 2 || local["notes/a.md"] == "" {
		t.Fatalf("HashStore = %v, want only the two notes", local)
	}
	seen := map[string]string{"notes/a.md": local["notes/a.md"] + " peerhash"}
	if got := changedSince(local, seen); !slices.Equal(got, []string{"notes/b.md"}) {
		t.Fatalf("ChangedSince = %v, want [notes/b.md]", got)
	}
}

func TestSyncLocalTokenTracksContent(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	write(t, Path("topic.md"), "one\n")
	before := localToken()
	write(t, Path("topic.md"), "two\n") // the same dirty-line count, other bytes
	if after := localToken(); after == before || after == "" {
		t.Fatalf("LocalToken did not move with the content: %q -> %q", before, after)
	}
}

func TestSyncRefusesInSandboxButNotInNormalDir(t *testing.T) {
	spawns := syncFixture(t, "on", "peer.example")
	t.Setenv("CLAUDE_REMOTE_HOST", "peer.example")
	now := time.Now()
	if !SyncOn() || !SyncIfDue(now, now, t.TempDir()) || spawns.Sync.Load() != 1 {
		t.Fatalf("a normal config dir did not reach the transport: spawns=%d", spawns.Sync.Load())
	}
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := recordSandbox(cfg); err != nil {
		t.Fatal(err)
	}
	if SyncOn() || SyncIfDue(now.Add(syncEvery*2), now.Add(syncEvery*2), t.TempDir()) || spawns.Sync.Load() != 1 {
		t.Fatalf("a sandbox config dir synced: spawns=%d", spawns.Sync.Load())
	}
}

// fakeEnvelopes is an in-memory EnvelopeStore: per-origin rows and watermarks, apply records the seq.
type fakeEnvelopes struct {
	rows    map[string][]Envelope
	acked   map[string]int64
	applied []int64
}

func (f *fakeEnvelopes) ChangesAfter(origin string, after int64, limit int) ([]Envelope, error) {
	var out []Envelope
	for _, e := range f.rows[origin] {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeEnvelopes) Watermark(origin string) (int64, int, error) { return f.acked[origin], 0, nil }
func (f *fakeEnvelopes) ApplyEnvelope(e Envelope) (bool, error) {
	f.applied, f.acked[e.Origin] = append(f.applied, e.Seq), max(f.acked[e.Origin], e.Seq)
	return false, nil
}
func (f *fakeEnvelopes) Conflicts(bool) ([]ConflictRow, error) { return nil, nil }
func (f *fakeEnvelopes) ResolveConflict(int64, string) error   { return nil }
func (f *fakeEnvelopes) Close() error                          { return nil }

func envLine(seq int64, schema int) string {
	return fmt.Sprintf(`{"origin":"peer","origin_seq":%d,"schema":%d,"entry_ref":"u","op":"upsert"}`+"\n", seq, schema)
}

// TestEnvelopeImportHolds: lines apply in seq order; a gap or a schema mismatch holds the origin unacknowledged.
func TestEnvelopeImportHolds(t *testing.T) {
	st := &fakeEnvelopes{acked: map[string]int64{}}
	rep, err := ImportEnvelopes(st, strings.NewReader(envLine(2, 2)+envLine(1, 2)+envLine(4, 2)), 2)
	if err != nil || !slices.Equal(st.applied, []int64{1, 2}) || len(rep.Held) != 1 || !strings.Contains(rep.Held[0], "gap") {
		t.Fatalf("ordered import = %+v, applied %v (%v)", rep, st.applied, err)
	}
	rep, err = ImportEnvelopes(st, strings.NewReader(envLine(3, 3)), 2)
	if err != nil || st.acked["peer"] != 2 || len(rep.Held) != 1 || !strings.Contains(rep.Held[0], "claude-code upgrade") {
		t.Fatalf("newer schema import = %+v, acked %d (%v)", rep, st.acked["peer"], err)
	}
	if _, err := ImportEnvelopes(st, strings.NewReader(`{"origin":"a/b","origin_seq":1,"schema":2}`), 2); err == nil {
		t.Fatal("an origin with a slash was accepted")
	}
}

// TestEnvelopeExportOrder: export pages in seq order and refuses a store that skips a seq.
func TestEnvelopeExportOrder(t *testing.T) {
	st := &fakeEnvelopes{rows: map[string][]Envelope{"peer": {{Origin: "peer", Seq: 1}, {Origin: "peer", Seq: 2}}}}
	var b strings.Builder
	if n, err := ExportEnvelopes(st, "peer", 0, &b); err != nil || n != 2 || strings.Count(b.String(), "\n") != 2 {
		t.Fatalf("export = %d (%v): %q", n, err, b.String())
	}
	st.rows["peer"] = []Envelope{{Origin: "peer", Seq: 3}}
	if _, err := ExportEnvelopes(st, "peer", 1, &b); err == nil {
		t.Fatal("export accepted a skipped seq")
	}
}
