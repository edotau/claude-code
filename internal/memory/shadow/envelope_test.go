package shadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
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

func writeStoreFile(t *testing.T, root, rel, body string) {
	t.Helper()
	envOK(t, os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o600) == nil, "write %s", rel)
}

// rebuiltPeer is a peer whose db was imported from a fixture store: every entry has an empty origin (rendered_rev 1).
func rebuiltPeer(t *testing.T) (envPeer, string) {
	t.Helper()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	a := newEnvPeer(t)
	_, err := shadowRebuild(context.Background(), a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	return a, root
}

func TestEnvelopeSyncDropLeavesTombstone(t *testing.T) {
	a, root := rebuiltPeer(t)
	uidOf := func(q string) string {
		var u string
		envOK(t, a.db.QueryRow(q).Scan(&u) == nil, "uid of %s", q)
		return u
	}
	older := uidOf(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND lines LIKE '%Older one%'`)
	kept := uidOf(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = '0123abcd'`)
	report := uidOf(`SELECT uid FROM entries WHERE doc = 'reports/r.md'`)
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-15 — Pick zebrafish storage <!-- id:0123abcd -->\n\nWhy: speed.\n")
	envOK(t, os.Remove(filepath.Join(root, "reports", "r.md")) == nil, "remove report")
	_, err := shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	for _, u := range []string{older, report} {
		envOK(t, a.count(t, `SELECT count(*) FROM tombstones WHERE uid = ?`, u) == 1, "dropped entry %s left no tombstone", u)
	}
	envOK(t, a.count(t, `SELECT count(*) FROM tombstones WHERE uid = ?`, kept) == 0, "a kept entry was tombstoned")
	_, err = shadowRebuild(context.Background(), a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM tombstones`) == 2, "a full rebuild changed the tombstones")
	e := memory.Envelope{Origin: strings.Repeat("cd", 16), Seq: 1, Schema: memory.EnvelopeSchema, EntryRef: older,
		BaseVersion: "x/9", Op: memory.EnvelopeOpUpsert, Payload: []byte(`{"doc":"decisionLog.md","kind":"heading","lines":"back"}`)}
	conflict, err := a.st.ApplyEnvelope(e)
	envOK(t, err == nil && conflict, "a stale upsert resurrected a dropped entry: conflict=%v err=%v", conflict, err)
}

func TestEnvelopeFileEditOfEmptyOriginIsCaptured(t *testing.T) {
	a, root := rebuiltPeer(t)
	var uid string
	envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = '0123abcd'`).Scan(&uid) == nil, "uid")
	envOK(t, a.count(t, `SELECT count(*) FROM changes`) == 0, "an import captured a change")
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-15 — Pick zebrafish storage <!-- id:0123abcd -->\n\nWhy: speed and cost.\n\n## 2026-01-10 — Older one\n\nbody\n")
	_, err := shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	got, err := a.st.ChangesAfter(a.id, 0, 10)
	envOK(t, err == nil && len(got) == 1 && got[0].EntryRef == uid && got[0].BaseVersion == "" && got[0].Op == "upsert",
		"changes after an outside edit = %+v (%v), want one upsert over the empty base", got, err)
	writeStoreFile(t, root, "notes/new.md", "fresh\n") // adoption of a new doc stays uncaptured
	_, err = shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil && a.count(t, `SELECT count(*) FROM changes`) == 1, "adopting a new doc captured a change (%v)", err)
}

func TestEnvelopeGuardImportOfEmptyOriginIsCaptured(t *testing.T) {
	a, root := rebuiltPeer(t)
	edb := &exportDB{db: a.db, root: root}
	cur, err := edb.Render("conventions.md")
	envOK(t, err == nil, "render: %v", err)
	sum := sha256.Sum256(cur)
	writeStoreFile(t, root, "conventions.md", "# Conventions\n\n- use gofmt -s\n- wrap errors\n  continued\n")
	envOK(t, edb.Import("conventions.md", hex.EncodeToString(sum[:])) == nil, "guard import")
	got, err := a.st.ChangesAfter(a.id, 0, 10)
	envOK(t, err == nil && len(got) == 1 && got[0].BaseVersion == "" && strings.Contains(string(got[0].Payload), "gofmt -s"),
		"changes after a guard import = %+v (%v)", got, err)
}

const decisionEntry = "## 2026-01-15 — Pick zebrafish storage <!-- id:0123abcd -->\n\nWhy: speed.\n"

func TestEnvelopeHarnessRewritesCaptureNothing(t *testing.T) {
	a, root := rebuiltPeer(t)
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n"+decisionEntry+
		"\n## 2026-01-10 — Older one <!-- id:deadbeef -->\n\nbody\n")
	writeStoreFile(t, root, "conventions.md", "# Conventions\n\n- use gofmt\n- wrap errors continued\n")
	_, err := shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM changes`) == 0, "an id stamp or a re-wrap was captured as a local change")
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE id = 'deadbeef' AND uid IS NOT NULL`) == 1, "stamped entry lost its row")
	writeStoreFile(t, root, "conventions.md", "# Conventions\n\n- use gofmt\n- wrap errors differently\n") // negative control
	_, err = shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil && a.count(t, `SELECT count(*) FROM changes`) == 1, "a real edit was not captured (%v)", err)
}

func TestEnvelopeFullRebuildCapturesNothing(t *testing.T) {
	a, root := rebuiltPeer(t)
	writeStoreFile(t, root, "conventions.md", "# Conventions\n\n- use gofmt\n- wrap errors differently\n")
	_, err := shadowRebuild(context.Background(), a.db, root)
	envOK(t, err == nil && a.count(t, `SELECT count(*) FROM changes`) == 0, "a full rebuild captured a change (%v)", err)
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE lines LIKE '%differently%'`) == 1, "rebuild did not take the file")
}

func TestEnvelopeRotatedEntryKeepsUID(t *testing.T) {
	a, root := rebuiltPeer(t)
	var uid string
	envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = '0123abcd'`).Scan(&uid) == nil, "uid")
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-10 — Older one\n\nbody\n")
	writeStoreFile(t, root, "archive/decisionLog-2026-10.md", "# Decision Log archive\n\n## 2026-10-01 — Archived\n\nold\n\n"+decisionEntry)
	_, err := shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND doc = 'archive/decisionLog-2026-10.md' AND deleted = 0`, uid) == 1,
		"a rotated entry lost its uid")
	envOK(t, a.count(t, `SELECT count(*) FROM tombstones WHERE uid = ?`, uid) == 0, "a rotated entry was tombstoned")
	got, err := a.st.ChangesAfter(a.id, 0, 10)
	envOK(t, err == nil && len(got) == 1 && got[0].EntryRef == uid && strings.Contains(string(got[0].Payload), "archive/"),
		"the move is not one upsert under the same uid: %+v (%v)", got, err)
}

func TestEnvelopeGuardImportIgnoresIDStamp(t *testing.T) {
	a, root := rebuiltPeer(t)
	edb := &exportDB{db: a.db, root: root}
	cur, err := edb.Render("decisionLog.md")
	envOK(t, err == nil, "render: %v", err)
	sum := sha256.Sum256(cur)
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n"+decisionEntry+
		"\n## 2026-01-10 — Older one <!-- id:deadbeef -->\n\nbody\n")
	envOK(t, edb.Import("decisionLog.md", hex.EncodeToString(sum[:])) == nil, "guard import")
	envOK(t, a.count(t, `SELECT count(*) FROM changes`) == 0, "a guard import captured an id stamp")
}

func TestEnvelopeStampedEntryKeepsUID(t *testing.T) {
	a, root := rebuiltPeer(t)
	var uid string
	envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND lines LIKE '%Older one%'`).Scan(&uid) == nil, "uid")
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n"+decisionEntry+
		"\n## 2026-01-10 — Older one <!-- id:deadbeef -->\n\nbody\n")
	_, err := shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND id = 'deadbeef'`, uid) == 1, "a stamped entry got a new uid")
	envOK(t, a.count(t, `SELECT count(*) FROM tombstones`) == 0, "a stamped entry tombstoned its old uid")
}

func TestEnvelopeCanonIDMatchRunsBeforeStampMatch(t *testing.T) {
	a, root := rebuiltPeer(t)
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-10 — Dup\n\nbody\n\n## 2026-01-10 — Dup <!-- id:deadbeef -->\n\nbody\n")
	_, err := shadowRebuild(context.Background(), a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	var uidPlain, uidStamped string
	envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = ''`).Scan(&uidPlain) == nil, "uidPlain")
	envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = 'deadbeef'`).Scan(&uidStamped) == nil, "uidStamped")
	writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-10 — Dup <!-- id:cafebabe -->\n\nbody\n\n## 2026-01-10 — Dup <!-- id:deadbeef -->\n\n  body\n")
	_, err = shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "sync: %v", err)
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND id = 'deadbeef'`, uidStamped) == 1, "re-wrapped entry lost its uid to stamp match")
	envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND id = 'cafebabe'`, uidPlain) == 1, "newly stamped entry did not take the unstamped uid")
	envOK(t, a.count(t, `SELECT count(*) FROM changes`) == 0, "id stamp and re-wrap captured a change")
}

func TestEnvelopeGuardRotatedEntryKeepsUID(t *testing.T) {
	for _, target := range []string{"archive/decisionLog-2026-10.md", "sessionHistory.md"} {
		a, root := rebuiltPeer(t)
		edb := &exportDB{db: a.db, root: root}
		rep, err := memory.ExportGuardLocked(edb, root)
		envOK(t, err == nil && len(rep.Conflicts) == 0, "initial guard: %+v (%v)", rep, err)
		var uid string
		envOK(t, a.db.QueryRow(`SELECT uid FROM entries WHERE doc = 'decisionLog.md' AND id = '0123abcd'`).Scan(&uid) == nil, "uid")
		writeStoreFile(t, root, "decisionLog.md", "# Decision Log\n\n> Dated decisions.\n\n## 2026-01-10 — Older one\n\nbody\n")
		writeStoreFile(t, root, target, "# Target\n\n## 2026-10-01 — Archived\n\nold\n\n"+decisionEntry)
		rep, err = memory.ExportGuardLocked(edb, root)
		envOK(t, err == nil && len(rep.Conflicts) == 0, "guard rotate to %s: %+v (%v)", target, rep, err)
		envOK(t, a.count(t, `SELECT count(*) FROM entries WHERE uid = ? AND doc = ? AND deleted = 0`, uid, target) == 1,
			"guard move to %s lost the entry's uid", target)
		got, err := a.st.ChangesAfter(a.id, 0, 10)
		envOK(t, err == nil && slices.ContainsFunc(got, func(e memory.Envelope) bool {
			return e.EntryRef == uid && e.Op == "upsert" && strings.Contains(string(e.Payload), target)
		}) && !slices.ContainsFunc(got, func(e memory.Envelope) bool {
			return e.EntryRef == uid && e.Op == "delete"
		}), "guard move to %s changes = %+v (%v)", target, got, err)
	}
}
