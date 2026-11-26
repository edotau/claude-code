package shadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/sqlite"
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
	rep, err := memory.ImportEnvelopes(to.st, &buf, memory.EnvelopeMaxSchema)
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

// writeBank rewrites a bank doc via writeDoc; an item is a kept uid, "uid=text" (kept, new lines) or new text.
func (p envPeer) writeBank(t *testing.T, name string, items ...string) {
	t.Helper()
	ctx := context.Background()
	err := sqlite.Tx(ctx, p.db, func(tx *sql.Tx) error {
		st, err := loadBank(ctx, tx)
		if err != nil {
			return err
		}
		next := st.docs[name]
		next.Entries = nil
		for _, it := range items {
			e := memory.DocEntry{Kind: memory.KindBullet}
			uid, text, kept := strings.Cut(it, "=")
			if !entryUIDRE.MatchString(it) && (!kept || !entryUIDRE.MatchString(uid)) {
				uid, text = "", it
			} else if !kept {
				uid = it
			}
			if uid != "" {
				var lines string
				if err := tx.QueryRowContext(ctx, `SELECT rowid, lines FROM entries WHERE uid = ?`, uid).Scan(&e.Row, &lines); err != nil {
					return err
				}
				if text == "" {
					text = strings.TrimPrefix(lines, "- ")
				}
			}
			e.Lines = []string{"- " + text}
			next.Entries = append(next.Entries, e)
		}
		return writeDoc(ctx, tx, name, st, next)
	})
	envOK(t, err == nil, "writeBank: %v", err)
}

// order is doc's live uids in seq order.
func (p envPeer) order(t *testing.T, doc string) []string {
	t.Helper()
	rows, err := p.db.Query(`SELECT uid FROM entries WHERE doc = ? AND deleted = 0 ORDER BY seq`, doc)
	envOK(t, err == nil, "order: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		envOK(t, rows.Scan(&u) == nil, "scan")
		out = append(out, u)
	}
	return out
}

func (p envPeer) orderRows(t *testing.T, doc string) int {
	t.Helper()
	return p.count(t, `SELECT count(*) FROM changes WHERE op = 'order' AND doc_ref = ?`, doc)
}

const orderDoc = "progress.md"

// sharedOrder is a and b holding the same three bullets, synced; it returns their uids in order.
func sharedOrder(t *testing.T) (envPeer, envPeer, []string) {
	t.Helper()
	a, b := newEnvPeer(t), newEnvPeer(t)
	a.writeBank(t, orderDoc, "x", "y", "z")
	meshSync(t, a, b)
	u := a.order(t, orderDoc)
	envOK(t, len(u) == 3 && slices.Equal(u, b.order(t, orderDoc)), "shared order a=%q b=%q", u, b.order(t, orderDoc))
	return a, b, u
}

func TestEnvelopeOrderReorderSyncs(t *testing.T) {
	a, b, u := sharedOrder(t)
	envOK(t, a.orderRows(t, orderDoc) == 1, "first write order rows = %d, want 1", a.orderRows(t, orderDoc))
	a.writeBank(t, orderDoc, u[2], u[0], u[1])
	envOK(t, a.orderRows(t, orderDoc) == 2, "reorder order rows = %d, want 2", a.orderRows(t, orderDoc))
	meshSync(t, a, b)
	want := []string{u[2], u[0], u[1]}
	envOK(t, slices.Equal(b.order(t, orderDoc), want), "b order = %q, want %q", b.order(t, orderDoc), want)
	envOK(t, b.count(t, `SELECT count(*) FROM conflicts`) == 0, "reorder filed a conflict")
	envOK(t, b.count(t, `SELECT count(*) FROM changes WHERE op = 'order' AND origin = ?`, b.id) == 0, "b captured an order change for an applied one")
}

func TestEnvelopeOrderInsertMiddleOneRow(t *testing.T) {
	a, _, u := sharedOrder(t)
	before := a.count(t, `SELECT count(*) FROM changes`)
	a.writeBank(t, orderDoc, u[0], "new", u[1], u[2])
	envOK(t, a.count(t, `SELECT count(*) FROM changes`)-before == 2 && a.orderRows(t, orderDoc) == 2,
		"insert gave %d change rows, want upsert plus one order", a.count(t, `SELECT count(*) FROM changes`)-before)
}

func TestEnvelopeOrderDeleteEmitsNone(t *testing.T) {
	a, _, u := sharedOrder(t)
	before := a.count(t, `SELECT count(*) FROM changes`)
	a.writeBank(t, orderDoc, u[0], u[2])
	envOK(t, a.count(t, `SELECT count(*) FROM changes`)-before == 1 && a.orderRows(t, orderDoc) == 1,
		"delete gave %d change rows, want only its tombstone", a.count(t, `SELECT count(*) FROM changes`)-before)
}

func TestEnvelopeOrderIDStampEmitsNothing(t *testing.T) {
	a, _, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[0]+"=x <!-- id:abcd1234 -->", u[1], u[2])
	envOK(t, a.orderRows(t, orderDoc) == 1, "an id stamp emitted an order change")
}

func TestEnvelopeOrderConcurrentAppendsMerge(t *testing.T) {
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[0], u[1], u[2], "from a")
	b.writeBank(t, orderDoc, u[0], u[1], u[2], "from b")
	meshSync(t, a, b)
	oa, ob := a.order(t, orderDoc), b.order(t, orderDoc)
	envOK(t, len(oa) == 5 && slices.Equal(oa, ob) && slices.Equal(oa[:3], u), "merged a=%q b=%q", oa, ob)
	envOK(t, a.count(t, `SELECT count(*) FROM conflicts`)+b.count(t, `SELECT count(*) FROM conflicts`) == 0, "appends filed a conflict")
}

func TestEnvelopeOrderConcurrentReordersConflictOnce(t *testing.T) {
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[2], u[1], u[0])
	b.writeBank(t, orderDoc, u[1], u[0], u[2])
	meshSync(t, a, b)
	for _, p := range []envPeer{a, b} {
		n := p.count(t, `SELECT count(*) FROM conflicts WHERE op = 'order' AND doc = ? AND resolved = 0`, orderDoc)
		envOK(t, n == 1, "%s order conflicts = %d, want 1", p.id, n)
	}
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{u[2], u[1], u[0]}), "a lost its own order: %q", a.order(t, orderDoc))
	rows, err := a.st.Conflicts(true)
	envOK(t, err == nil && len(rows) == 1 && rows[0].Op == memory.EnvelopeOpOrder && rows[0].Doc == orderDoc, "listed = %+v (%v)", rows, err)
}

func TestEnvelopeOrderRebuildKeepsDocOrders(t *testing.T) {
	a, _, _ := sharedOrder(t)
	var before string
	envOK(t, a.db.QueryRow(`SELECT uids FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&before) == nil, "no doc_orders row")
	root := t.TempDir()
	writeShadowStore(t, root, false)
	_, err := shadowRebuild(context.Background(), a.db, root)
	envOK(t, err == nil, "rebuild: %v", err)
	var after string
	envOK(t, a.db.QueryRow(`SELECT uids FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&after) == nil && after == before,
		"doc_orders after rebuild = %q, want %q", after, before)
	envOK(t, a.orderRows(t, orderDoc) == 1, "rebuild dropped or added order changes: %d", a.orderRows(t, orderDoc))
}

func orderEnv(origin string, seq int64, base string, uids ...string) memory.Envelope {
	payload, _ := json.Marshal(orderPayload{UIDs: uids})
	return memory.Envelope{Origin: origin, Seq: seq, Schema: memory.EnvelopeOrderSchema,
		DocRef: orderDoc, BaseVersion: base, Op: memory.EnvelopeOpOrder, Payload: payload}
}

func TestEnvelopeOrderLateEntryAndSameProjection(t *testing.T) {
	a := newEnvPeer(t)
	u, v := strings.Repeat("a", 32), strings.Repeat("b", 32)
	e := orderEnv("order-peer", 1, "", v, u)
	conflict, err := a.st.ApplyEnvelope(e)
	envOK(t, err == nil && !conflict, "order: %v %v", conflict, err)
	for i, uid := range []string{u, v} {
		p, _ := json.Marshal(envelopePayload{Doc: orderDoc, Seq: i, Kind: "bullet", Lines: uid})
		_, err := a.st.ApplyEnvelope(memory.Envelope{Origin: "content-peer", Seq: int64(i + 1),
			Schema: memory.EnvelopeSchema, EntryRef: uid, Op: memory.EnvelopeOpUpsert, Payload: p})
		envOK(t, err == nil, "upsert: %v", err)
	}
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{v, u}), "late entries lost order: %v", a.order(t, orderDoc))
	// The same array must repair projection too, not merely update its stamp.
	execShadow(t, a.db, `UPDATE entries SET seq = seq + 10`)
	execShadow(t, a.db, `UPDATE entries SET seq = 20 - seq`)
	e.Origin = "same-peer"
	_, err = a.st.ApplyEnvelope(e)
	envOK(t, err == nil && slices.Equal(a.order(t, orderDoc), []string{v, u}), "same-state projection: %v", err)
}

func TestEnvelopeOrderRejectsUnrelatedBaseAndSyntheticSuccessor(t *testing.T) {
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[0], u[1], u[2], "append a")
	b.writeBank(t, orderDoc, u[0], u[1], u[2], "append b")
	meshSync(t, a, b)
	before := a.order(t, orderDoc)
	var stampOrigin string
	var stampSeq int64
	envOK(t, a.db.QueryRow(`SELECT origin, origin_seq FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&stampOrigin, &stampSeq) == nil, "load stamp")
	var raw string
	envOK(t, a.db.QueryRow(`SELECT payload FROM changes WHERE origin = ? AND origin_seq = ?`, stampOrigin, stampSeq).Scan(&raw) == nil, "load representative")
	var p orderPayload
	envOK(t, json.Unmarshal([]byte(raw), &p) == nil, "decode representative")
	for _, e := range []memory.Envelope{
		orderEnv("stale-empty-base", 1, "", before[2], before[0], before[1]),
		orderEnv("synthetic-successor", 1, baseOf(stampOrigin, stampSeq), append(p.UIDs, strings.Repeat("f", 32))...),
	} {
		conflict, err := a.st.ApplyEnvelope(e)
		envOK(t, err == nil && conflict && slices.Equal(a.order(t, orderDoc), before), "unsafe order accepted: %+v (%v, %v)", e, conflict, err)
	}
}

func TestEnvelopeOrderThreeConcurrentAppends(t *testing.T) {
	a, b, u := sharedOrder(t)
	c := newEnvPeer(t)
	meshSync(t, a, b, c)
	for i, p := range []envPeer{a, b, c} {
		p.writeBank(t, orderDoc, u[0], u[1], u[2], fmt.Sprintf("append %d", i))
	}
	meshSync(t, a, b, c)
	want := a.order(t, orderDoc)
	for _, p := range []envPeer{a, b, c} {
		envOK(t, len(want) == 6 && slices.Equal(want, p.order(t, orderDoc)), "three-way order: %v vs %v", want, p.order(t, orderDoc))
		envOK(t, p.count(t, `SELECT count(*) FROM conflicts`) == 0, "three-way append conflicted")
	}
}

func TestEnvelopeOrderConflictSemanticDedupAndConvergence(t *testing.T) {
	a, _, u := sharedOrder(t)
	e := orderEnv("fork", 1, "missing/1", u[2], u[1], u[0])
	conflict, err := a.st.ApplyEnvelope(e)
	envOK(t, err == nil && conflict, "initial conflict: %v %v", conflict, err)
	e.Payload = json.RawMessage(fmt.Sprintf(`{ "uids" : [ %q, %q, %q ] }`, u[2], u[1], u[0]))
	_, err = a.st.ApplyEnvelope(e) // seq-mismatch path must not file the same variant under another kind
	envOK(t, err == nil && a.count(t, `SELECT count(*) FROM conflicts WHERE op = 'order'`) == 1, "duplicate conflict: %v", err)
	e = orderEnv("fork", 2, "fork/1", u...)
	conflict, err = a.st.ApplyEnvelope(e)
	envOK(t, err == nil && !conflict && a.count(t, `SELECT count(*) FROM conflicts WHERE resolved = 0`) == 0,
		"same-state convergence left blocked conflict: %v %v", conflict, err)
}

func TestEnvelopeOrderRollbackAndNegativeSlots(t *testing.T) {
	a, _, u := sharedOrder(t)
	// Old negative positions (including a live row without a UID) must not collide with temporary -rowid slots.
	execShadow(t, a.db, `UPDATE entries SET seq = seq + 10`)
	execShadow(t, a.db, `UPDATE entries SET seq = -2 WHERE seq = 10`)
	var before string
	envOK(t, a.db.QueryRow(`SELECT uids FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&before) == nil, "order state")
	execShadow(t, a.db, `CREATE TRIGGER reject_order BEFORE INSERT ON changes WHEN new.op = 'order'
		BEGIN SELECT RAISE(ABORT, 'reject order'); END`)
	var base string
	envOK(t, a.db.QueryRow(`SELECT origin || '/' || origin_seq FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&base) == nil, "base")
	e := orderEnv("rollback-peer", 1, base, u[2], u[1], u[0])
	_, err := a.st.ApplyEnvelope(e)
	envOK(t, err != nil, "expected injected failure")
	var after string
	envOK(t, a.db.QueryRow(`SELECT uids FROM doc_orders WHERE doc = ?`, orderDoc).Scan(&after) == nil && before == after, "failed apply changed order")
	acked, _, err := a.st.Watermark(e.Origin)
	envOK(t, err == nil && acked == 0 && a.count(t, `SELECT count(*) FROM changes WHERE origin = ?`, e.Origin) == 0, "failed apply recorded change")
	execShadow(t, a.db, `DROP TRIGGER reject_order`)
	_, err = a.st.ApplyEnvelope(e)
	envOK(t, err == nil && slices.Equal(a.order(t, orderDoc), []string{u[2], u[1], u[0]}), "negative slot projection: %v", err)
}

func TestEnvelopeOrderPayloadRequiresArray(t *testing.T) {
	a := newEnvPeer(t)
	for _, raw := range []string{`null`, `{}`, `{"uids":null}`} {
		e := orderEnv("bad-order", 1, "")
		e.Payload = json.RawMessage(raw)
		_, err := a.st.ApplyEnvelope(e)
		envOK(t, err != nil, "accepted malformed payload %s", raw)
	}
}

func TestEnvelopeOrderSuccessorOfMergedState(t *testing.T) {
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[0], u[1], u[2], "append a")
	b.writeBank(t, orderDoc, u[0], u[1], u[2], "append b")
	meshSync(t, a, b)
	merged := a.order(t, orderDoc)
	a.writeBank(t, orderDoc, merged[4], merged[3], merged[2], merged[1], merged[0])
	meshSync(t, a, b)
	envOK(t, slices.Equal(a.order(t, orderDoc), b.order(t, orderDoc)), "successor lost merged order")
	envOK(t, b.count(t, `SELECT count(*) FROM conflicts`) == 0, "successor of merged state conflicted")
	// The merged array can also be the common base of another append fork.
	u = a.order(t, orderDoc)
	a.writeBank(t, orderDoc, append(u, "next a")...)
	b.writeBank(t, orderDoc, append(slices.Clone(u), "next b")...)
	meshSync(t, a, b)
	envOK(t, slices.Equal(a.order(t, orderDoc), b.order(t, orderDoc)) && len(a.order(t, orderDoc)) == 7,
		"subsequent append fork did not merge")
}

func TestEnvelopePeersSchemaKeepsMaximum(t *testing.T) {
	a := newEnvPeer(t)
	u := strings.Repeat("a", 32)
	p, _ := json.Marshal(envelopePayload{Doc: orderDoc, Kind: "bullet", Lines: u})
	for _, e := range []memory.Envelope{orderEnv("schema-peer", 1, "", u), {Origin: "schema-peer", Seq: 2, Schema: memory.EnvelopeSchema,
		EntryRef: u, Op: memory.EnvelopeOpUpsert, Payload: p}} {
		_, err := a.st.ApplyEnvelope(e)
		envOK(t, err == nil, "apply %d: %v", e.Seq, err)
	}
	_, schema, err := a.st.Watermark("schema-peer")
	envOK(t, err == nil && schema == memory.EnvelopeOrderSchema, "peers.schema = %d (%v), want the maximum", schema, err)
}

func TestEnvelopeOrderTravelsOnSchemaThree(t *testing.T) {
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[2], u[1], u[0])
	envOK(t, a.count(t, `SELECT count(*) FROM changes WHERE op = 'order' AND schema = ?`, memory.EnvelopeOrderSchema) >= 1, "order not on schema 3")
	var buf bytes.Buffer
	_, err := memory.ExportEnvelopes(a.st, a.id, 0, &buf)
	envOK(t, err == nil, "export: %v", err)
	old, err := memory.ImportEnvelopes(b.st, bytes.NewReader(buf.Bytes()), memory.EnvelopeSchema)
	envOK(t, err == nil && len(old.Held) == 1 && slices.Equal(b.order(t, orderDoc), u), "old reader: held %v (%v)", old.Held, err)
	bad := orderEnv("x", 1, "")
	bad.Schema = memory.EnvelopeSchema
	_, err = b.st.ApplyEnvelope(bad)
	envOK(t, err != nil, "order on schema 2 accepted")
}

// orderConflictPeers is a and b holding different reorders of three shared bullets, a's conflict open.
func orderConflictPeers(t *testing.T) (envPeer, envPeer, []string, int64) {
	t.Helper()
	a, b, u := sharedOrder(t)
	a.writeBank(t, orderDoc, u[2], u[1], u[0])
	b.writeBank(t, orderDoc, u[1], u[0], u[2])
	meshSync(t, a, b)
	rows, err := a.st.Conflicts(true)
	envOK(t, err == nil && len(rows) == 1, "open order conflicts = %+v (%v)", rows, err)
	return a, b, u, rows[0].ID
}

func TestEnvelopeOrderResolveKeepPeer(t *testing.T) {
	a, _, u, id := orderConflictPeers(t)
	before := a.orderRows(t, orderDoc)
	envOK(t, a.st.ResolveConflict(id, "peer") == nil, "resolve peer")
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{u[1], u[0], u[2]}), "peer order not applied: %q", a.order(t, orderDoc))
	envOK(t, a.orderRows(t, orderDoc) == before+1, "keep peer emitted %d order rows", a.orderRows(t, orderDoc)-before)
	envOK(t, a.count(t, `SELECT count(*) FROM conflicts WHERE op = 'order' AND resolved = 0`) == 0, "conflict left open")
	envOK(t, a.count(t, `SELECT count(*) FROM conflicts WHERE op = 'order' AND kind = 'replaced'`) == 1, "replaced order not kept")
}

func TestEnvelopeOrderResolveKeepLocal(t *testing.T) {
	a, _, u, id := orderConflictPeers(t)
	before := a.orderRows(t, orderDoc)
	envOK(t, a.st.ResolveConflict(id, "local") == nil, "resolve local")
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{u[2], u[1], u[0]}), "local order lost: %q", a.order(t, orderDoc))
	envOK(t, a.orderRows(t, orderDoc) == before+1, "keep local emitted %d order rows", a.orderRows(t, orderDoc)-before)
	envOK(t, a.st.ResolveConflict(id, "local") != nil, "resolved conflict resolved twice")
}

func TestEnvelopeGuardImportReorderIsCaptured(t *testing.T) {
	a, b, u := sharedOrder(t)
	root := t.TempDir()
	edb := &exportDB{db: a.db, root: root}
	cur, err := edb.Render(orderDoc)
	envOK(t, err == nil, "render: %v", err)
	sum := sha256.Sum256(cur)
	lines := strings.Split(string(cur), "\n")
	var at []int
	for i, l := range lines {
		if strings.HasPrefix(l, "- ") {
			at = append(at, i)
		}
	}
	envOK(t, len(at) == 3, "rendered bullets = %d", len(at))
	lines[at[0]], lines[at[2]] = lines[at[2]], lines[at[0]]
	writeStoreFile(t, root, orderDoc, strings.Join(lines, "\n"))
	before := a.orderRows(t, orderDoc)
	envOK(t, edb.Import(orderDoc, hex.EncodeToString(sum[:])) == nil, "guard import")
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{u[2], u[1], u[0]}), "import order = %q", a.order(t, orderDoc))
	envOK(t, a.orderRows(t, orderDoc) == before+1, "guard reorder emitted %d order rows", a.orderRows(t, orderDoc)-before)
	meshSync(t, a, b)
	envOK(t, slices.Equal(b.order(t, orderDoc), a.order(t, orderDoc)), "peer order = %q", b.order(t, orderDoc))
	// the same file again is a settle-style no-op: no new order change
	cur, err = edb.Render(orderDoc)
	envOK(t, err == nil, "render again: %v", err)
	sum = sha256.Sum256(cur)
	writeStoreFile(t, root, orderDoc, string(cur))
	envOK(t, edb.Import(orderDoc, hex.EncodeToString(sum[:])) == nil, "second guard import")
	envOK(t, a.orderRows(t, orderDoc) == before+1, "unchanged import emitted an order row")
}

func TestEnvelopeOrderWaitsForMissingUIDs(t *testing.T) {
	a := newEnvPeer(t)
	u, v := strings.Repeat("a", 32), strings.Repeat("b", 32)
	_, err := a.st.ApplyEnvelope(orderEnv("order-peer", 1, "", v, u))
	envOK(t, err == nil && a.count(t, `SELECT count(*) FROM doc_orders WHERE doc = ?`, orderDoc) == 1, "order not stored: %v", err)
	for i, uid := range []string{u, v} { // u lands first, v later from another origin
		p, _ := json.Marshal(envelopePayload{Doc: orderDoc, Seq: 0, Kind: "bullet", Lines: uid})
		_, err := a.st.ApplyEnvelope(memory.Envelope{Origin: "content-" + uid[:1], Seq: 1, Schema: memory.EnvelopeSchema,
			EntryRef: uid, Op: memory.EnvelopeOpUpsert, Payload: p})
		envOK(t, err == nil, "upsert %d: %v", i, err)
	}
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{v, u}), "waiting uid not placed: %v", a.order(t, orderDoc))
}

func TestEnvelopeShadowSyncReorderIsCaptured(t *testing.T) {
	a, b, u := sharedOrder(t)
	root := t.TempDir()
	writeShadowStore(t, root, false)
	_, err := a.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, metaImported, importMarker())
	envOK(t, err == nil, "meta: %v", err)
	body := "# Progress\n\n" + a.lines(t, u[2]) + "\n" + a.lines(t, u[1]) + "\n" + a.lines(t, u[0]) + "\n"
	writeStoreFile(t, root, orderDoc, body)
	before := a.orderRows(t, orderDoc)
	_, err = shadowSync(context.Background(), a.db, root, false)
	envOK(t, err == nil, "shadowSync: %v", err)
	envOK(t, slices.Equal(a.order(t, orderDoc), []string{u[2], u[1], u[0]}), "sync order = %q", a.order(t, orderDoc))
	envOK(t, a.orderRows(t, orderDoc) == before+1, "shadowSync reorder emitted %d order rows", a.orderRows(t, orderDoc)-before)
	meshSync(t, a, b)
	envOK(t, slices.Equal(b.order(t, orderDoc), a.order(t, orderDoc)), "peer order = %q", b.order(t, orderDoc))
}
