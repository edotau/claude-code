package shadow

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

// envPeer is one machine's temp db with its envelope store; the lock and truth seams never touch the live store.
type envPeer struct {
	db *sql.DB
	st sqlEnvelopeStore
	id string
}

func newEnvPeer(t *testing.T) envPeer {
	t.Helper()
	db := openShadowDB(t, nil)
	st := sqlEnvelopeStore{db: db, lock: func(f func() error) error { return f() }, truthDB: func() (bool, error) { return true, nil }}
	var id string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'machine_id'`).Scan(&id)
	envOK(t, err == nil && len(id) == 32, "machine_id = %q (%v)", id, err)
	return envPeer{db: db, st: st, id: id}
}

// envOK fails the test with the message when ok is false.
func envOK(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}

// localAdd writes an entry as a local store write does (rendered_rev 0) and returns its uid.
func (p envPeer) localAdd(t *testing.T, doc string, seq int, lines string) string {
	t.Helper()
	execShadow(t, p.db, `INSERT OR IGNORE INTO docs(path, kind, header, grammar) VALUES('`+doc+`', 'note', '', 'note')`)
	_, err := p.db.Exec(`INSERT INTO entries(doc, id, seq, kind, lines, date, version, rendered_rev)
		VALUES(?, '', ?, 'file', ?, '', 1, 0)`, doc, seq, lines)
	var uid string
	if err == nil {
		err = p.db.QueryRow(`SELECT uid FROM entries WHERE doc = ? AND seq = ?`, doc, seq).Scan(&uid)
	}
	envOK(t, err == nil, "local add: %v", err)
	return uid
}

func (p envPeer) localEdit(t *testing.T, uid, lines string) {
	t.Helper()
	_, err := p.db.Exec(`UPDATE entries SET lines = ?, version = version + 1 WHERE uid = ?`, lines, uid)
	envOK(t, err == nil, "local edit: %v", err)
}

func (p envPeer) lines(t *testing.T, uid string) string {
	t.Helper()
	var s string
	err := p.db.QueryRow(`SELECT lines FROM entries WHERE uid = ? AND deleted = 0`, uid).Scan(&s)
	envOK(t, err == nil, "entry %s: %v", uid, err)
	return s
}

func (p envPeer) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	err := p.db.QueryRow(q, args...).Scan(&n)
	envOK(t, err == nil, "%s: %v", q, err)
	return n
}

// send exports every change from origin after afterSeq and imports it into to.
func send(t *testing.T, from, to envPeer, origin string, afterSeq int64) memory.ImportReport {
	t.Helper()
	var buf bytes.Buffer
	_, err := memory.ExportEnvelopes(from.st, origin, afterSeq, &buf)
	envOK(t, err == nil, "export: %v", err)
	rep, err := memory.ImportEnvelopes(to.st, &buf, memory.EnvelopeSchema)
	envOK(t, err == nil, "import: %v", err)
	return rep
}

func TestEnvelopeCaptureAndExport(t *testing.T) {
	a := newEnvPeer(t)
	uid := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, uid, "two")
	got, err := a.st.ChangesAfter(a.id, 0, 10)
	envOK(t, err == nil && len(got) == 2, "changes = %+v (%v), want 2", got, err)
	envOK(t, got[0].Seq == 1 && got[0].BaseVersion == "" && got[1].Seq == 2 && got[1].BaseVersion == a.id+"/1" &&
		got[1].EntryRef == uid, "captured changes out of order or unbased: %+v", got)
	envOK(t, strings.Contains(string(got[1].Payload), `"lines":"two"`), "payload = %s", got[1].Payload)
	var buf bytes.Buffer
	n, err := memory.ExportEnvelopes(a.st, a.id, 1, &buf)
	envOK(t, err == nil && n == 1, "export after 1 = %d (%v)", n, err)
}

func TestEnvelopeImportOrderAndGapHold(t *testing.T) {
	a, b, c := newEnvPeer(t), newEnvPeer(t), newEnvPeer(t)
	uid := a.localAdd(t, "notes/a.md", 0, "one")
	a.localEdit(t, uid, "two")
	rep := send(t, a, b, a.id, 0)
	envOK(t, rep.Applied == 2 && rep.Conflicts == 0, "import = %+v", rep)
	envOK(t, b.lines(t, uid) == "two" && b.count(t, `SELECT count(*) FROM changes WHERE origin = ?`, b.id) == 0,
		"peer apply missing or captured as a local change")
	rep = send(t, a, b, a.id, 0)
	envOK(t, rep.Dup == 2 && rep.Applied == 0, "repeat import = %+v, want 2 duplicates", rep)
	rep = send(t, a, c, a.id, 1) // seq 2 without seq 1: held
	envOK(t, len(rep.Held) == 1 && rep.Applied == 0 && c.count(t, `SELECT count(*) FROM entries`) == 0, "gap import = %+v", rep)
	acked, _, _ := c.st.Watermark(a.id)
	envOK(t, acked == 0, "held import moved the watermark to %d", acked)
}

func TestEnvelopeConflictAndResolve(t *testing.T) {
	a, b := newEnvPeer(t), newEnvPeer(t)
	uid := a.localAdd(t, "notes/a.md", 0, "base")
	send(t, a, b, a.id, 0)
	a.localEdit(t, uid, "from a")
	a.localEdit(t, uid, "from a again")
	b.localEdit(t, uid, "from b")
	rep := send(t, a, b, a.id, 1)
	envOK(t, rep.Conflicts == 2 && rep.Applied == 2, "diverged import = %+v", rep)
	rows, err := b.st.Conflicts(true)
	envOK(t, err == nil && len(rows) == 2 && rows[0].Kind == conflictDiverged && rows[1].Kind == conflictBlocked &&
		rows[0].Op == "upsert", "conflicts = %+v (%v)", rows, err)
	envOK(t, b.lines(t, uid) == "from b", "a conflicting envelope overwrote the local entry")
	envOK(t, b.st.ResolveConflict(rows[1].ID, "peer") != nil, "resolved a later conflict before the older one")
	envOK(t, b.st.ResolveConflict(rows[0].ID, "local") == nil, "keep local failed")
	envOK(t, b.st.ResolveConflict(rows[1].ID, "peer") == nil, "keep peer failed")
	envOK(t, b.lines(t, uid) == "from a again", "keep peer left %q", b.lines(t, uid))
	mine, _ := b.st.ChangesAfter(b.id, 0, 10) // b's edit, then two resolutions
	envOK(t, len(mine) == 3 && mine[2].BaseVersion == a.id+"/3" && mine[2].Op == "upsert", "resolution envelopes = %+v", mine)
	send(t, b, a, b.id, 0) // b's last envelope equals a's state: a converges and closes b's superseded conflicts
	open, _ := a.st.Conflicts(true)
	envOK(t, a.lines(t, uid) == "from a again" && len(open) == 0, "a after b's changes = %q, open %+v", a.lines(t, uid), open)
}

func TestEnvelopeResolvedDeleteLeavesTombstone(t *testing.T) {
	b := newEnvPeer(t)
	uid := strings.Repeat("ab", 16)
	e := memory.Envelope{Origin: strings.Repeat("cd", 16), Seq: 1, Schema: memory.EnvelopeSchema, EntryRef: uid,
		BaseVersion: "x/1", Op: memory.EnvelopeOpDelete, At: "t"}
	conflict, err := b.st.ApplyEnvelope(e)
	envOK(t, err == nil && conflict, "unbased delete = %v (%v), want a conflict", conflict, err)
	rows, _ := b.st.Conflicts(true)
	envOK(t, len(rows) == 1 && rows[0].Op == "delete", "conflicts = %+v", rows)
	envOK(t, b.st.ResolveConflict(rows[0].ID, "peer") == nil, "keep peer failed")
	envOK(t, b.count(t, `SELECT count(*) FROM tombstones WHERE uid = ?`, uid) == 1, "a resolved delete left no tombstone")
	mine, _ := b.st.ChangesAfter(b.id, 0, 10)
	envOK(t, len(mine) == 1 && mine[0].Op == "delete", "resolution envelope = %+v", mine)
}

func TestEnvelopeFilesTruthRefused(t *testing.T) {
	b := newEnvPeer(t)
	b.st.truthDB = func() (bool, error) { return false, nil }
	e := memory.Envelope{Origin: "peer", Seq: 1, Schema: memory.EnvelopeSchema, EntryRef: strings.Repeat("ab", 16),
		Op: memory.EnvelopeOpUpsert, Payload: []byte(`{"doc":"notes/a.md","kind":"file","lines":"x"}`)}
	_, err := b.st.ApplyEnvelope(e)
	envOK(t, err != nil, "applied an envelope in files truth")
	envOK(t, b.count(t, `SELECT (SELECT count(*) FROM entries) + (SELECT count(*) FROM peers)`) == 0, "a refused envelope wrote rows")
}

// TestEnvelopeRebuildKeepsEveryChangesRow: a full rebuild keeps changes, peers and each entry's uid.
func TestEnvelopeRebuildKeepsEveryChangesRow(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	a := newEnvPeer(t)
	_, err := shadowRebuild(ctx, a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	var uid string
	err = a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = '0123abcd'`).Scan(&uid)
	envOK(t, err == nil, "entry uid: %v", err)
	a.localAdd(t, "notes/z.md", 0, "kept")
	execShadow(t, a.db, `UPDATE entries SET version = version + 1 WHERE uid = '`+uid+`'`,
		`INSERT INTO peers(origin, acked_seq, schema) VALUES('peer', 7, 2)`)
	before := a.count(t, `SELECT count(*) FROM changes`)
	envOK(t, before == 2, "captured %d changes, want 2", before)
	_, err = shadowRebuild(ctx, a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM changes`) == before && a.count(t, `SELECT acked_seq FROM peers WHERE origin = 'peer'`) == 7,
		"a rebuild pruned changes or peer watermarks")
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND origin = ?`, uid, a.id) == 1,
		"a rebuild lost the entry's uid or its last change")
}
