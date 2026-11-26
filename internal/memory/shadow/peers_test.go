package shadow

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// docsOf is a peer's live entries as sorted uid|doc|lines rows: what convergence compares.
func docsOf(t *testing.T, p envPeer) []string {
	t.Helper()
	rows, err := p.db.Query(`SELECT uid, doc, lines FROM entries WHERE deleted = 0 ORDER BY uid`)
	envOK(t, err == nil, "docs: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u, d, l string
		envOK(t, rows.Scan(&u, &d, &l) == nil, "scan")
		out = append(out, u+"|"+d+"|"+l)
	}
	envOK(t, rows.Err() == nil, "docs: %v", rows.Err())
	return out
}

// meshSync runs SyncPair between every ordered pair until a round moves nothing, and fails if it never settles.
func meshSync(t *testing.T, peers ...envPeer) {
	t.Helper()
	for range 6 {
		if meshRound(t, peers) == 0 {
			return
		}
	}
	t.Fatal("mesh did not settle in 6 rounds")
}

// meshRound is one SyncPair per ordered pair; it returns how many envelopes moved.
func meshRound(t *testing.T, peers []envPeer) int {
	t.Helper()
	moved := 0
	for _, from := range peers {
		for _, to := range peers {
			if to.id == from.id {
				continue
			}
			var origins []string
			for _, o := range peers {
				if o.id != to.id {
					origins = append(origins, o.id)
				}
			}
			rep, err := SyncPair(context.Background(), from.st, to.st, origins)
			envOK(t, err == nil && len(rep.Held) == 0 && len(rep.Errors) == 0, "SyncPair: %+v (%v)", rep, err)
			moved += rep.Applied + rep.Dup + rep.Conflicts
		}
	}
	return moved
}

func sameDocs(t *testing.T, peers ...envPeer) {
	t.Helper()
	want := docsOf(t, peers[0])
	for i, p := range peers[1:] {
		envOK(t, slices.Equal(want, docsOf(t, p)), "peer %d docs differ:\n%q\n%q", i+1, want, docsOf(t, p))
	}
}

func TestThreePeerFanIn(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	ua := a.localAdd(t, "notes/a.md", 0, "from a")
	b.localAdd(t, "notes/b.md", 0, "from b")
	c.localAdd(t, "notes/c.md", 0, "from c")
	a.localEdit(t, ua, "from a, edited")
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
	envOK(t, meshRound(t, []envPeer{a, b, c}) == 0, "a second round with no new writes moved envelopes")
	envOK(t, len(docsOf(t, c)) == 3 && c.lines(t, ua) == "from a, edited", "fan-in docs = %q", docsOf(t, c))
	for _, p := range []envPeer{a, b, c} {
		w, _, _ := p.st.Watermark(a.id)
		envOK(t, p.id == a.id || w == 2, "watermark of a on %s = %d, want 2", p.id, w)
		envOK(t, p.count(t, `SELECT count(*) FROM conflicts`) == 0, "unexpected conflict rows")
	}
}

func TestThreePeerGapHeldUntilFilled(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	a.localEdit(t, u, "three")
	send(t, a, b, a.id, 0)
	rep := send(t, b, c, a.id, 2) // b relays seq 3 only: c has nothing yet
	envOK(t, len(rep.Held) == 1 && rep.Applied == 0 && len(docsOf(t, c)) == 0, "gap relay = %+v", rep)
	rep, err := SyncPair(context.Background(), b.st, c.st, []string{a.id})
	envOK(t, err == nil && rep.Applied == 3 && len(rep.Held) == 0, "fill gap = %+v (%v)", rep, err)
	envOK(t, c.lines(t, u) == "three", "after fill c has %q", c.lines(t, u))
	w, _, _ := c.st.Watermark(a.id)
	envOK(t, w == 3, "watermark = %d, want 3", w)
	sameDocs(t, a, b, c)
}

func TestThreePeerConflictRow(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "base")
	meshSync(t, a, b, c)
	a.localEdit(t, u, "edit on a")
	b.localEdit(t, u, "edit on b")
	meshSync(t, a, b, c)
	for _, p := range []envPeer{a, b} {
		rows, err := p.st.Conflicts(true)
		envOK(t, err == nil && len(rows) == 1 && rows[0].Kind == conflictDiverged && rows[0].EntryRef == u,
			"conflicts on %s = %+v (%v)", p.id, rows, err)
	}
	envOK(t, a.lines(t, u) == "edit on a" && b.lines(t, u) == "edit on b", "a conflict overwrote a local edit")
	envOK(t, c.count(t, `SELECT count(*) FROM conflicts WHERE resolved = 0`) == 1, "c did not record exactly one conflict")
	rows, _ := b.st.Conflicts(true)
	envOK(t, b.st.ResolveConflict(rows[0].ID, "peer") == nil, "keep peer failed")
	meshSync(t, a, b, c)
	envOK(t, b.lines(t, u) == "edit on a", "b after resolve = %q", b.lines(t, u))
}

func TestThreePeerLateJoiner(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	b.localAdd(t, "notes/b.md", 0, "bee")
	a.localEdit(t, u, "two")
	meshSync(t, a, b)
	c := newEnvPeer(t)
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
	envOK(t, len(docsOf(t, c)) == 2 && c.lines(t, u) == "two", "late joiner docs = %q", docsOf(t, c))
	c.localAdd(t, "notes/c.md", 0, "late")
	meshSync(t, a, b, c)
	sameDocs(t, a, b, c)
}

func TestBootstrapCopyMintsMachineID(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := sqlite.Open(srcPath, sqlite.Options{})
	envOK(t, err == nil, "open: %v", err)
	_, err = migrateMemoryDB(ctx, src, sqlite.MigrateOptions{SnapshotDir: t.TempDir()})
	envOK(t, err == nil, "migrate: %v", err)
	a := envPeer{db: src, st: sqlEnvelopeStore{db: src, lock: func(f func() error) error { return f() },
		truthDB: func() (bool, error) { return true, nil }}}
	envOK(t, a.db.QueryRow(`SELECT value FROM meta WHERE key = 'machine_id'`).Scan(&a.id) == nil, "src id")
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	_, err = src.Exec(`CREATE TABLE IF NOT EXISTS embed_claim(id INTEGER PRIMARY KEY CHECK(id = 1), expiry INTEGER, owner INTEGER)`)
	envOK(t, err == nil, "claim table: %v", err)
	t.Cleanup(func() { _ = src.Close() })
	_, err = src.Exec(`INSERT OR REPLACE INTO embed_claim(id, expiry, owner) VALUES(1, ?, 7)`, int64(math.MaxInt64))
	envOK(t, err == nil, "claim: %v", err)
	dst := freshDest(t)
	id, err := BootstrapCopy(ctx, srcPath, dst)
	envOK(t, err == nil && id != a.id && len(id) == 32, "bootstrap id %q (%v), source %q", id, err, a.id)
	db, err := sqlite.Open(dst, sqlite.Options{})
	envOK(t, err == nil, "open copy: %v", err)
	t.Cleanup(func() { _ = db.Close() })
	b := envPeer{db: db, id: id, st: sqlEnvelopeStore{db: db, lock: a.st.lock, truthDB: a.st.truthDB}}
	var got string
	envOK(t, db.QueryRow(`SELECT value FROM meta WHERE key = 'machine_id'`).Scan(&got) == nil && got == id, "copy id = %q", got)
	envOK(t, slices.Equal(docsOf(t, a), docsOf(t, b)), "copy docs differ")
	w, _, _ := b.st.Watermark(a.id)
	envOK(t, w == 2, "copy watermark for the source = %d, want 2", w)
	b.localAdd(t, "notes/b.md", 0, "new peer") // its first change is seq 1 under the new id
	meshSync(t, a, b)
	sameDocs(t, a, b)
	envOK(t, b.count(t, `SELECT count(*) FROM conflicts`) == 0 && a.count(t, `SELECT count(*) FROM conflicts`) == 0,
		"bootstrap replay filed conflicts")
	var n int
	envOK(t, db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'embed_claim'`).Scan(&n) == nil, "claim table")
	envOK(t, n == 1, "claim table missing in copy")
	envOK(t, db.QueryRow(`SELECT count(*) FROM embed_claim`).Scan(&n) == nil && n == 0, "copy kept embed_claim")
	_, err = copyPeer(ctx, srcPath, dst)
	envOK(t, err != nil && strings.Contains(err.Error(), "exists"), "second copy over dst = %v", err)
	_, err = BootstrapCopy(ctx, filepath.Join(dir, "none.db"), freshDest(t))
	envOK(t, err != nil, "missing source copied")
}

func TestSyncPairIsIncremental(t *testing.T) {
	ctx := context.Background()
	a, b := newEnvPeer(t), newEnvPeer(t)
	u := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, u, "two")
	rep, err := SyncPair(ctx, a.st, b.st, []string{a.id, a.id})
	envOK(t, err == nil && rep.Applied == 2 && rep.Dup == 0, "first = %+v (%v)", rep, err)
	rep, err = SyncPair(ctx, a.st, b.st, []string{a.id})
	envOK(t, err == nil && rep.Applied+rep.Dup+rep.Conflicts == 0, "repeat moved %+v (%v)", rep, err)
	a.localEdit(t, u, "three")
	rep, err = SyncPair(ctx, a.st, b.st, []string{a.id})
	envOK(t, err == nil && rep.Applied == 1 && rep.Dup == 0, "delta = %+v (%v)", rep, err)
	sameDocs(t, a, b)
}

func TestBootstrapCopyLeavesRacingDestination(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	src, err := sqlite.Open(srcPath, sqlite.Options{})
	envOK(t, err == nil, "open: %v", err)
	_, err = migrateMemoryDB(ctx, src, sqlite.MigrateOptions{SnapshotDir: t.TempDir()})
	envOK(t, err == nil, "migrate: %v", err)
	envOK(t, src.Close() == nil, "close")
	dst := freshDest(t)
	orig := linkNoReplace
	t.Cleanup(func() { linkNoReplace = orig })
	linkNoReplace = func(o, n string) error {
		ran, err := paths.TryFileLock(memory.BankLockPath(), 0, func() error { return nil })
		envOK(t, err == nil && !ran, "bootstrap did not hold bank lock: %v", err)
		envOK(t, os.WriteFile(n, []byte("theirs"), 0o600) == nil, "racer")
		return orig(o, n)
	}
	_, err = BootstrapCopy(ctx, srcPath, dst)
	envOK(t, err != nil, "copy over a racing dst succeeded")
	got, rerr := os.ReadFile(dst)
	envOK(t, rerr == nil && string(got) == "theirs", "racing dst changed: %q (%v)", got, rerr)
	left, _ := filepath.Glob(dst + ".tmp-*")
	envOK(t, len(left) == 0, "temp files left: %v", left)
}

func TestSyncPairCanceled(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	a.localAdd(t, "notes/a.md", 0, "one")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, origins := range [][]string{nil, {a.id}} {
		_, err := SyncPair(ctx, a.st, b.st, origins)
		envOK(t, errors.Is(err, context.Canceled), "canceled sync: %v", err)
	}
	envOK(t, len(docsOf(t, b)) == 0, "canceled sync wrote rows")
}
