package shadow

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/sqlite"
)

const (
	conflictDiverged    = "diverged"     // the peer changed a base this side has moved past
	conflictBlocked     = "blocked"      // the entry already had an open conflict: queued behind it
	conflictSeqMismatch = "seq-mismatch" // a repeated (origin, seq) with a different change
	machineQ            = `(SELECT value FROM meta WHERE key = 'machine_id')`
)

// keepTombstones moves deleted rows into tombstones before an import drops them; callers append the WHERE.
const keepTombstones = `INSERT OR IGNORE INTO tombstones(uid, origin, origin_seq, at)
	SELECT uid, origin, origin_seq, NULL FROM entries`

// captureSQL records a local write as our next changes row (schema 2, frozen) and stamps the entry, in the writer's tx.
func captureSQL(base string) string {
	return `UPDATE entries SET uid = COALESCE(uid, lower(hex(randomblob(16)))) WHERE rowid = new.rowid;
	INSERT INTO changes(origin, origin_seq, schema, entry_rowid, entry_uid, base_version, op, payload, at)
	SELECT m.value, COALESCE((SELECT MAX(c.origin_seq) FROM changes c WHERE c.origin = m.value), 0) + 1, 2, new.rowid,
	(SELECT uid FROM entries WHERE rowid = new.rowid), ` + base + `, CASE WHEN new.deleted THEN 'delete' ELSE 'upsert' END,
	CASE WHEN new.deleted THEN '' ELSE json_object('doc', new.doc, 'id', new.id, 'seq', new.seq, 'kind', new.kind,
	'lines', new.lines, 'date', new.date) END, strftime('%Y-%m-%dT%H:%M:%SZ', 'now') FROM meta m WHERE m.key = 'machine_id';
	UPDATE entries SET origin = ` + machineQ + `, origin_seq = (SELECT MAX(origin_seq) FROM changes
	WHERE origin = ` + machineQ + `) WHERE rowid = new.rowid;`
}

// entryUIDV5 adds uids and captures local writes as changes rows; imports (rendered_rev 1) and peer applies never.
func entryUIDV5(tx *sql.Tx) (string, error) {
	for _, q := range []string{
		`ALTER TABLE entries ADD COLUMN uid TEXT`,
		`UPDATE entries SET uid = lower(hex(randomblob(16)))`,
		`CREATE UNIQUE INDEX entries_uid ON entries(uid)`,
		`DROP TRIGGER entries_au`, // re-scoped so uid and origin stamps never touch FTS
		`CREATE TRIGGER entries_au AFTER UPDATE OF doc, lines, area ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, lines, area, doc) VALUES('delete', old.rowid, old.lines, old.area, old.doc);
		INSERT INTO entries_fts(rowid, lines, area, doc) VALUES(new.rowid, new.lines, new.area, new.doc); END`,
		`CREATE TRIGGER entries_uid_ai AFTER INSERT ON entries WHEN new.uid IS NULL BEGIN
		UPDATE entries SET uid = COALESCE(uid, lower(hex(randomblob(16)))) WHERE rowid = new.rowid; END`,
		`ALTER TABLE changes ADD COLUMN entry_uid TEXT`,
		`ALTER TABLE conflicts ADD COLUMN entry_uid TEXT`,
		`ALTER TABLE conflicts ADD COLUMN op TEXT NOT NULL DEFAULT 'upsert'`,
		`ALTER TABLE conflicts ADD COLUMN base_version TEXT`,
		`UPDATE changes SET entry_uid = (SELECT uid FROM entries WHERE rowid = changes.entry_rowid)`,
		`UPDATE conflicts SET entry_uid = (SELECT uid FROM entries WHERE rowid = conflicts.entry_rowid) WHERE entry_rowid <> 0`,
		`CREATE TABLE tombstones(uid TEXT PRIMARY KEY, origin TEXT NOT NULL, origin_seq INTEGER NOT NULL, at TEXT)`,
		`INSERT OR IGNORE INTO meta(key, value) VALUES('machine_id', lower(hex(randomblob(16))))`,
		`CREATE TRIGGER entries_capture_ai AFTER INSERT ON entries WHEN new.origin = '' AND new.rendered_rev = 0
		BEGIN ` + captureSQL("''") + ` END`,
		`CREATE TRIGGER entries_capture_au AFTER UPDATE OF version ON entries WHEN new.version <> old.version
		AND new.origin IS old.origin AND new.origin_seq = old.origin_seq BEGIN ` +
			captureSQL(`CASE WHEN old.origin = '' THEN '' ELSE old.origin || '/' || old.origin_seq END`) + ` END`,
	} {
		if _, err := tx.ExecContext(context.Background(), q); err != nil {
			return "", err
		}
	}
	return "entry uid, tombstones, local change capture", nil
}

// sqlEnvelopeStore applies envelopes to the db truth; lock and truthDB are seams so tests never touch the live store.
type sqlEnvelopeStore struct {
	db      *sql.DB
	lock    func(func() error) error
	truthDB func() (bool, error)
	export  func() error // markdown export after a commit, under the same lock; nil in tests
	schema  int          // 0 means memory.EnvelopeMaxSchema; tests set EnvelopeSchema to simulate an older peer
}

var _ memory.EnvelopeStore = sqlEnvelopeStore{}

func init() { memory.OpenEnvelopeStore = openEnvelopeStore }

func openEnvelopeStore() (memory.EnvelopeStore, error) {
	db, err := openMigratedShadow(context.Background())
	if err != nil {
		return nil, err
	}
	lock := func(f func() error) error { return memory.WithBankLock(memory.BankLockWait, f) }
	truth := func() (bool, error) { return true, nil } // the db is the only store since 7.2 H1
	return sqlEnvelopeStore{db: db, lock: lock, truthDB: truth, export: exportAfterWrite}, nil
}

func (s sqlEnvelopeStore) Close() error { return s.db.Close() }

// write runs f in one transaction under the bank lock, refused unless the db is the truth (one truth at a time).
func (s sqlEnvelopeStore) write(f func(context.Context, *sql.Tx) error) error {
	return s.lock(func() error {
		ok, err := s.truthDB()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("memory envelopes: memory truth is files; switch with: claude-code memory store --truth db")
		}
		ctx := context.Background()
		err = sqlite.Tx(ctx, s.db, func(tx *sql.Tx) error {
			if err := f(ctx, tx); err != nil {
				return err
			}
			return execEntryCount(ctx, tx)
		})
		if err != nil || s.export == nil {
			return err
		}
		if xerr := s.export(); xerr != nil {
			return fmt.Errorf("memory envelopes: committed in the db; markdown export: %w", xerr)
		}
		return nil
	})
}

func (s sqlEnvelopeStore) ChangesAfter(origin string, afterSeq int64, limit int) ([]memory.Envelope, error) {
	if limit <= 0 {
		return nil, errors.New("memory envelopes: limit must be positive")
	}
	rows, err := s.db.QueryContext(context.Background(), `SELECT origin, origin_seq, schema, COALESCE(entry_uid, ''),
		COALESCE(doc_ref, ''), COALESCE(base_version, ''), op, COALESCE(payload, ''), COALESCE(at, '') FROM changes
		WHERE origin = ? AND origin_seq > ? ORDER BY origin_seq LIMIT ?`, origin, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.Envelope
	for rows.Next() {
		var e memory.Envelope
		var payload string
		if err := rows.Scan(&e.Origin, &e.Seq, &e.Schema, &e.EntryRef, &e.DocRef, &e.BaseVersion, &e.Op, &payload, &e.At); err != nil {
			return nil, err
		}
		if payload != "" {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s sqlEnvelopeStore) Watermark(origin string) (int64, int, error) {
	return ackedSeq(context.Background(), s.db, origin)
}

var entryUIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validEnvelope(e memory.Envelope) error {
	if e.Op == memory.EnvelopeOpOrder {
		return validOrder(e)
	}
	if !entryUIDRE.MatchString(e.EntryRef) {
		return fmt.Errorf("memory envelopes: invalid entry ref %q", e.EntryRef)
	}
	return validEnvelopeOp(e)
}

// ApplyEnvelope is one transaction: apply or file a conflict, record the changes row, advance the watermark.
func (s sqlEnvelopeStore) ApplyEnvelope(e memory.Envelope) (bool, error) {
	if err := checkApply(e); err != nil {
		return false, err
	}
	var conflict bool
	err := s.write(func(ctx context.Context, tx *sql.Tx) (err error) {
		conflict, err = applyEnvelopeTx(ctx, tx, e)
		return err
	})
	return conflict, err
}

// checkApply refuses an envelope this store cannot apply before any transaction opens.
func checkApply(e memory.Envelope) error {
	if e.Origin == "" || strings.Contains(e.Origin, "/") || e.Seq <= 0 || e.Schema != memory.OpSchema(e.Op) {
		return fmt.Errorf("memory envelopes: invalid envelope %q/%d schema %d", e.Origin, e.Seq, e.Schema)
	}
	return validEnvelope(e)
}

func applyEnvelopeTx(ctx context.Context, tx *sql.Tx, e memory.Envelope) (bool, error) {
	if e.Origin == machine(ctx, tx) { // our own change echoed back: already in changes, never re-applied
		return ackOwn(ctx, tx, e)
	}
	acked, _, err := ackedSeq(ctx, tx, e.Origin)
	if err != nil {
		return false, err
	}
	switch {
	case e.Seq <= acked:
		return duplicateEnvelope(ctx, tx, e)
	case e.Seq != acked+1:
		return false, fmt.Errorf("memory envelopes: gap for %q: have %d, got %d", e.Origin, acked, e.Seq)
	}
	conflict, err := applyVerdict(ctx, tx, e)
	if err != nil {
		return false, err
	}
	if err := insertEnvelopeChange(ctx, tx, e); err != nil {
		return false, err
	}
	return conflict, recordAck(ctx, tx, e)
}

// applyVerdict applies e, stamps an entry already in e's state, or files the conflict that keeps e apart.
func applyVerdict(ctx context.Context, tx *sql.Tx, e memory.Envelope) (bool, error) {
	if e.Op == memory.EnvelopeOpOrder {
		return applyOrder(ctx, tx, e)
	}
	kind, r, err := envelopeVerdict(ctx, tx, e)
	switch {
	case err != nil:
		return false, err
	case kind == "":
		return false, applyEnvelopeEntry(ctx, tx, r, e)
	case kind == "same":
		return false, convergeEntry(ctx, tx, e)
	}
	return true, insertEnvelopeConflict(ctx, tx, e, kind)
}

// convergeEntry: e's state is already ours, so stamp it and close the older conflicts from e.Origin it superseded.
func convergeEntry(ctx context.Context, tx *sql.Tx, e memory.Envelope) error {
	if err := stampEntry(ctx, tx, e.EntryRef, e.Origin, e.Seq); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE conflicts SET resolved = 1 WHERE entry_uid = ? AND peer_origin = ?
		AND peer_seq < ? AND resolved = 0`, e.EntryRef, e.Origin, e.Seq)
	return err
}

// ackOwn refuses an own seq this db never wrote (restored from an older copy), so a later capture cannot reuse it.
func ackOwn(ctx context.Context, tx *sql.Tx, e memory.Envelope) (bool, error) {
	var have bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM changes WHERE origin = ? AND origin_seq = ?)`,
		e.Origin, e.Seq).Scan(&have); err != nil {
		return false, err
	}
	if !have {
		return false, fmt.Errorf("memory envelopes: own change %s/%d is unknown here: this db is older than its peers", e.Origin, e.Seq)
	}
	conflict, err := duplicateEnvelope(ctx, tx, e) // a different change under our own seq is kept as a conflict
	if err != nil {
		return false, err
	}
	return conflict, recordAck(ctx, tx, e)
}

func machine(ctx context.Context, tx *sql.Tx) string {
	var id string
	_ = tx.QueryRowContext(ctx, machineQ[1:len(machineQ)-1]).Scan(&id) // step 5 always writes it
	return id
}

// envelopeVerdict is "" to apply, "same" when nothing would change, else the conflict kind to file.
func envelopeVerdict(ctx context.Context, tx *sql.Tx, e memory.Envelope) (string, entryState, error) {
	r, err := loadEntryState(ctx, tx, e.EntryRef)
	if err != nil || r.sameAs(e) {
		return "same", r, err
	}
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflicts WHERE entry_uid = ? AND resolved = 0)`,
		e.EntryRef).Scan(&open); err != nil || open {
		return conflictBlocked, r, err
	}
	if e.BaseVersion == r.base {
		return "", r, nil
	}
	return conflictDiverged, r, nil
}

// entryState is uid's row (rowid 0 when only a tombstone or nothing holds it) and the change it was last at.
type entryState struct {
	rowid    int64
	deleted  bool
	seq      int
	base     string
	payload  envelopePayload
	tombOnly bool
}

func baseOf(origin string, seq int64) string {
	if origin == "" {
		return ""
	}
	return origin + "/" + strconv.FormatInt(seq, 10)
}

func loadEntryState(ctx context.Context, tx *sql.Tx, uid string) (entryState, error) {
	var r entryState
	var origin string
	var oseq int64
	p := &r.payload
	err := tx.QueryRowContext(ctx, `SELECT rowid, deleted, origin, origin_seq, doc, id, seq, kind, lines, date
		FROM entries WHERE uid = ?`, uid).Scan(&r.rowid, &r.deleted, &origin, &oseq, &p.Doc, &p.ID, &r.seq, &p.Kind, &p.Lines, &p.Date)
	if err == nil {
		p.Seq, r.base = r.seq, baseOf(origin, oseq)
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	err = tx.QueryRowContext(ctx, `SELECT origin, origin_seq FROM tombstones WHERE uid = ?`, uid).Scan(&origin, &oseq)
	if errors.Is(err, sql.ErrNoRows) {
		return entryState{}, nil
	}
	r.deleted, r.tombOnly, r.base = true, true, baseOf(origin, oseq)
	return r, err
}

// sameAs reports that applying e changes nothing: already deleted, or the same live content (position aside).
func (r entryState) sameAs(e memory.Envelope) bool {
	if e.Op == memory.EnvelopeOpDelete {
		return r.deleted
	}
	var want envelopePayload
	if r.rowid == 0 || r.deleted || json.Unmarshal(e.Payload, &want) != nil {
		return false
	}
	want.Seq = r.payload.Seq
	return want == r.payload
}

func validEnvelopeOp(e memory.Envelope) error {
	switch e.Op {
	case memory.EnvelopeOpDelete:
		return nil
	case memory.EnvelopeOpUpsert:
		var p envelopePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.Kind == "" || p.Seq < 0 {
			return fmt.Errorf("memory envelopes: %q/%d: upsert payload needs doc, kind and a seq >= 0", e.Origin, e.Seq)
		}
		return validDoc(p.Doc)
	}
	return fmt.Errorf("memory envelopes: %q/%d: unknown op %q", e.Origin, e.Seq, e.Op)
}

// validDoc accepts a clean, local, slash-separated store path to a Markdown file.
func validDoc(doc string) error {
	if doc == "" || path.Clean(doc) != doc || path.IsAbs(doc) || strings.HasPrefix(doc, "../") || doc == ".." ||
		strings.Contains(doc, `\`) || !strings.HasSuffix(doc, ".md") {
		return fmt.Errorf("memory envelopes: invalid doc %q", doc)
	}
	return nil
}

func insertEnvelopeChange(ctx context.Context, tx *sql.Tx, e memory.Envelope) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO changes(origin, origin_seq, schema, entry_rowid, entry_uid, doc_ref, base_version,
		op, payload, at) VALUES(?1, ?2, ?3, COALESCE((SELECT rowid FROM entries WHERE uid = ?4), 0), NULLIF(?4, ''), NULLIF(?5, ''),
		?6, ?7, ?8, ?9)`, e.Origin, e.Seq, e.Schema, e.EntryRef, e.DocRef, e.BaseVersion, e.Op, string(e.Payload), e.At)
	return err
}

func recordAck(ctx context.Context, tx *sql.Tx, e memory.Envelope) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO peers(origin, acked_seq, schema) VALUES(?, ?, ?)
		ON CONFLICT(origin) DO UPDATE SET acked_seq = MAX(acked_seq, excluded.acked_seq), schema = MAX(schema, excluded.schema)`, e.Origin, e.Seq, e.Schema)
	return err
}

// ackedSeq is origin's watermark and schema through q, the db or a transaction; an unknown origin is zero.
func ackedSeq(ctx context.Context, q queryer, origin string) (int64, int, error) {
	var acked int64
	var schema int
	err := q.QueryRowContext(ctx, `SELECT acked_seq, schema FROM peers WHERE origin = ?`, origin).Scan(&acked, &schema)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return acked, schema, err
}

// duplicateEnvelope compares a repeat with its stored change; a different change under the same seq is a conflict.
func duplicateEnvelope(ctx context.Context, tx *sql.Tx, e memory.Envelope) (bool, error) {
	var uid, doc, base, op, payload string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(entry_uid, ''), COALESCE(doc_ref, ''), COALESCE(base_version, ''), op,
		COALESCE(payload, '') FROM changes WHERE origin = ? AND origin_seq = ?`, e.Origin, e.Seq).Scan(&uid, &doc, &base, &op, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || uid == e.EntryRef && doc == e.DocRef && base == e.BaseVersion && op == e.Op && samePayload(payload, e.Payload) {
		return false, err
	}
	return true, insertEnvelopeConflict(ctx, tx, e, conflictSeqMismatch)
}

// samePayload compares JSON by value, so a peer that re-serialises the same payload is not a conflict.
func samePayload(old string, p json.RawMessage) bool {
	var a, b any
	if json.Unmarshal([]byte(old), &a) != nil || json.Unmarshal(p, &b) != nil {
		return old == string(p)
	}
	return reflect.DeepEqual(a, b)
}

// insertEnvelopeConflict files each distinct variant once; only an equal repeat (op, base, payload) files nothing.
func insertEnvelopeConflict(ctx context.Context, tx *sql.Tx, e memory.Envelope, kind string) error {
	if e.Op == memory.EnvelopeOpOrder {
		return insertOrderConflict(ctx, tx, e, kind)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conflicts(entry_rowid, entry_uid, peer_origin, peer_seq, payload, at, kind, op,
		base_version, doc) SELECT NULL, ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, COALESCE(CASE WHEN json_valid(?4)
		THEN json_extract(?4, '$.doc') END, (SELECT doc FROM entries WHERE uid = ?1)) WHERE NOT EXISTS (SELECT 1 FROM
		conflicts WHERE peer_origin = ?2 AND peer_seq = ?3 AND kind = ?6 AND entry_uid = ?1 AND op = ?7
		AND COALESCE(base_version, '') = ?8 AND COALESCE(payload, '') = ?4)`,
		e.EntryRef, e.Origin, e.Seq, string(e.Payload), e.At, kind, e.Op, e.BaseVersion)
	return err
}

type envelopePayload struct {
	Doc   string `json:"doc"`
	ID    string `json:"id"`
	Seq   int    `json:"seq"`
	Kind  string `json:"kind"`
	Lines string `json:"lines"`
	Date  string `json:"date"`
}

func applyEnvelopeEntry(ctx context.Context, tx *sql.Tx, r entryState, e memory.Envelope) error {
	if e.Op == memory.EnvelopeOpDelete {
		return deleteEnvelopeEntry(ctx, tx, r, e)
	}
	if err := upsertEnvelopeEntry(ctx, tx, r, e); err != nil {
		return err
	}
	var p envelopePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	o, err := loadOrder(ctx, tx, p.Doc)
	if err != nil || o.uids == nil {
		return err
	}
	return projectOrder(ctx, tx, p.Doc, o.uids)
}

// stampEntry records that uid now stands at origin/seq without a version bump, so no local change is captured.
func stampEntry(ctx context.Context, tx *sql.Tx, uid, origin string, seq int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET origin = ?, origin_seq = ? WHERE uid = ?`, origin, seq, uid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE tombstones SET origin = ?, origin_seq = ? WHERE uid = ?`, origin, seq, uid)
	return err
}

// placeSeq is where an upsert lands: its slot, else its current seq in the same doc, else after the doc's last row.
func placeSeq(ctx context.Context, tx *sql.Tx, r entryState, uid string, p envelopePayload) (int, error) {
	var taken bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM entries WHERE doc = ? AND seq = ? AND uid <> ?
		AND deleted = 0)`, p.Doc, p.Seq, uid).Scan(&taken); err != nil || !taken {
		return p.Seq, err
	}
	if r.rowid != 0 && r.payload.Doc == p.Doc && r.seq >= 0 {
		return r.seq, nil
	}
	var next int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) + 1 FROM entries WHERE doc = ?`, p.Doc).Scan(&next)
	return next, err
}

func upsertEnvelopeEntry(ctx context.Context, tx *sql.Tx, r entryState, e memory.Envelope) error {
	var p envelopePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return err
	}
	if err := ensureEnvelopeDoc(ctx, tx, p.Doc); err != nil {
		return err
	}
	seq, err := placeSeq(ctx, tx, r, e.EntryRef, p)
	if err != nil {
		return err
	}
	// a deleted row keeps its seq under the unique (doc, seq) index; move it aside as writeDoc does
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = -rowid WHERE doc = ? AND seq = ? AND uid <> ? AND deleted = 1`,
		p.Doc, seq, e.EntryRef); err != nil {
		return err
	}
	if r.rowid != 0 {
		_, err := tx.ExecContext(ctx, `UPDATE entries SET doc = ?, id = ?, seq = ?, kind = ?, lines = ?, date = ?, area = ?,
			deleted = 0, version = version + 1, origin = ?, origin_seq = ? WHERE rowid = ?`,
			p.Doc, p.ID, seq, p.Kind, p.Lines, p.Date, deriveArea(p.Lines), e.Origin, e.Seq, r.rowid)
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tombstones WHERE uid = ?`, e.EntryRef); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO entries(uid, doc, id, seq, kind, lines, date, area, version, origin, origin_seq)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(version), 0) + 1 FROM entries), ?, ?)`,
		e.EntryRef, p.Doc, p.ID, seq, p.Kind, p.Lines, p.Date, deriveArea(p.Lines), e.Origin, e.Seq)
	return err
}

// ensureEnvelopeDoc adds an empty docs row for a doc this side lacks, typed by where it lives.
func ensureEnvelopeDoc(ctx context.Context, tx *sql.Tx, doc string) error {
	kind, grammar := "note", "note"
	d := memory.ParseDoc(doc, nil)
	first, _, _ := strings.Cut(doc, "/")
	switch {
	case slices.Contains(memory.BankFiles(), doc):
		kind, grammar = "bank", grammarNames[d.Grammar]
	case first == "archive":
		kind, grammar = "archive", "file"
	case slices.Contains(shadowRecordDirs, first):
		kind, grammar = "record", "record"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO docs(path, kind, header, grammar) VALUES(?, ?, ?, ?)
		ON CONFLICT(path) DO NOTHING`, doc, kind, d.Header, grammar)
	return err
}

// deleteEnvelopeEntry marks the row deleted (the row is its tombstone) or files a tombstone for a uid with no row.
func deleteEnvelopeEntry(ctx context.Context, tx *sql.Tx, r entryState, e memory.Envelope) error {
	if r.rowid != 0 {
		_, err := tx.ExecContext(ctx, `UPDATE entries SET deleted = 1, version = version + 1,
			origin = ?, origin_seq = ? WHERE rowid = ?`, e.Origin, e.Seq, r.rowid)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO tombstones(uid, origin, origin_seq, at) VALUES(?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET origin = excluded.origin, origin_seq = excluded.origin_seq, at = excluded.at`,
		e.EntryRef, e.Origin, e.Seq, e.At)
	return err
}

// Conflicts lists envelope conflicts (export conflicts are `memory shadow --conflicts`), oldest first.
func (s sqlEnvelopeStore) Conflicts(openOnly bool) ([]memory.ConflictRow, error) {
	query := `SELECT rowid, COALESCE(entry_uid, ''), peer_origin, peer_seq, COALESCE(payload, ''), COALESCE(at, ''), resolved, kind,
		COALESCE(doc, ''), op FROM conflicts WHERE (entry_uid IS NOT NULL OR op = 'order')`
	if openOnly {
		query += ` AND resolved = 0`
	}
	rows, err := s.db.QueryContext(context.Background(), query+` ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory.ConflictRow
	for rows.Next() {
		var r memory.ConflictRow
		var payload string
		if err := rows.Scan(&r.ID, &r.EntryRef, &r.PeerOrigin, &r.PeerSeq, &payload, &r.At, &r.Resolved, &r.Kind, &r.Doc, &r.Op); err != nil {
			return nil, err
		}
		if payload != "" {
			r.Payload = json.RawMessage(payload)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveConflict keeps one side in one transaction and emits this side's resolved state as its own envelope.
func (s sqlEnvelopeStore) ResolveConflict(id int64, keep string) error {
	if keep != "local" && keep != "peer" {
		return errors.New("memory conflicts: keep must be local or peer")
	}
	return s.write(func(ctx context.Context, tx *sql.Tx) error {
		e, err := openConflict(ctx, tx, id)
		if err != nil {
			return err
		}
		if e.Op == memory.EnvelopeOpOrder {
			return resolveOrder(ctx, tx, id, keep, e)
		}
		if keep == "peer" {
			if err := keepPeer(ctx, tx, id, e); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE conflicts SET resolved = 1 WHERE rowid = ?`, id); err != nil {
			return err
		}
		return emitResolution(ctx, tx, e.EntryRef, baseOf(e.Origin, e.Seq))
	})
}

// openConflict loads open conflict id as the peer envelope it holds.
func openConflict(ctx context.Context, tx *sql.Tx, id int64) (memory.Envelope, error) {
	var e memory.Envelope
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(entry_uid, ''), COALESCE(payload, ''), peer_origin, peer_seq, op, COALESCE(at, ''),
		COALESCE(doc, '') FROM conflicts WHERE rowid = ? AND resolved = 0 AND (entry_uid IS NOT NULL OR op = 'order')`, id).
		Scan(&e.EntryRef, &payload, &e.Origin, &e.Seq, &e.Op, &e.At, &e.DocRef)
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("memory conflicts: no open conflict %d", id)
	}
	if payload != "" {
		e.Payload = json.RawMessage(payload)
	}
	return e, err
}

// keepPeer applies the conflict's peer change; an older open conflict on the entry must be resolved first.
func keepPeer(ctx context.Context, tx *sql.Tx, id int64, e memory.Envelope) error {
	var older bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflicts WHERE entry_uid = ? AND resolved = 0
		AND rowid < ?)`, e.EntryRef, id).Scan(&older); err != nil {
		return err
	}
	if older {
		return fmt.Errorf("memory conflicts: resolve the older conflict on entry %s first", e.EntryRef)
	}
	r, err := loadEntryState(ctx, tx, e.EntryRef)
	if err != nil {
		return err
	}
	if err := keepReplaced(ctx, tx, r, e.EntryRef); err != nil {
		return err
	}
	// unstamp first: an apply landing on the same origin/seq would read as a local write and be captured twice
	if err := stampEntry(ctx, tx, e.EntryRef, "", 0); err != nil {
		return err
	}
	return applyEnvelopeEntry(ctx, tx, r, e)
}

// keepReplaced files the local state keep-peer overwrites as a resolved "replaced" row, so neither side is lost.
func keepReplaced(ctx context.Context, tx *sql.Tx, r entryState, uid string) error {
	op, payload := memory.EnvelopeOpDelete, []byte(nil)
	if r.rowid != 0 && !r.deleted {
		op, payload = memory.EnvelopeOpUpsert, mustJSON(r.payload)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conflicts(entry_rowid, entry_uid, peer_origin, peer_seq, payload, at, kind, op,
		doc, resolved) VALUES(NULL, ?, ?, 0, ?, ?, 'replaced', ?, ?, 1)`, uid, machine(ctx, tx), string(payload),
		time.Now().UTC().Format(time.RFC3339), op, r.payload.Doc)
	return err
}

func mustJSON(p any) []byte {
	b, _ := json.Marshal(p) // a struct of strings and an int always marshals
	return b
}

// emitResolution writes this machine's next change for uid's resolved state, made over base, and stamps the entry.
func emitResolution(ctx context.Context, tx *sql.Tx, uid, base string) error {
	me := machine(ctx, tx)
	if me == "" {
		return errors.New("memory conflicts: no machine_id in meta")
	}
	r, err := loadEntryState(ctx, tx, uid)
	if err != nil || r.rowid == 0 && !r.tombOnly {
		return err // kept local and this side never held the entry: nothing to tell peers, never a delete
	}
	e := memory.Envelope{Origin: me, Schema: memory.EnvelopeSchema, EntryRef: uid, BaseVersion: base,
		Op: memory.EnvelopeOpDelete, At: time.Now().UTC().Format(time.RFC3339)}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(origin_seq), 0) + 1 FROM changes WHERE origin = ?`, me).Scan(&e.Seq); err != nil {
		return err
	}
	if !r.deleted {
		e.Op = memory.EnvelopeOpUpsert
		if e.Payload, err = json.Marshal(r.payload); err != nil {
			return err
		}
	}
	seq := e.Seq
	if err := insertEnvelopeChange(ctx, tx, e); err != nil {
		return err
	}
	return stampEntry(ctx, tx, uid, me, seq)
}

// orderV6 adds the per-document published entry order, shared as one order change per document.
func orderV6(tx *sql.Tx) (string, error) {
	for _, q := range []string{
		`CREATE TABLE doc_orders(doc TEXT PRIMARY KEY, uids TEXT NOT NULL, origin TEXT NOT NULL DEFAULT '',
		origin_seq INTEGER NOT NULL DEFAULT 0)`,
		`ALTER TABLE changes ADD COLUMN doc_ref TEXT`,
		`INSERT INTO doc_orders(doc, uids) SELECT doc, json_group_array(uid) FROM (SELECT doc, uid FROM entries
		WHERE deleted = 0 AND origin <> '' ORDER BY doc, seq) GROUP BY doc`,
	} {
		if _, err := tx.ExecContext(context.Background(), q); err != nil {
			return "", err
		}
	}
	return "document entry order", nil
}

// orderState is a document's last accepted published order and the change that set it.
type orderState struct {
	uids   []string
	origin string
	seq    int64
}

func (o orderState) base() string { return baseOf(o.origin, o.seq) }

func cmpStamp(a, b orderState) int {
	return cmp.Or(cmp.Compare(a.origin, b.origin), cmp.Compare(a.seq, b.seq))
}

func loadOrder(ctx context.Context, tx *sql.Tx, doc string) (orderState, error) {
	var o orderState
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT uids, origin, origin_seq FROM doc_orders WHERE doc = ?`, doc).Scan(&raw, &o.origin, &o.seq)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	return o, json.Unmarshal([]byte(raw), &o.uids)
}

func saveOrder(ctx context.Context, tx *sql.Tx, doc string, o orderState) error {
	b, err := json.Marshal(slices.Concat([]string{}, o.uids))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO doc_orders(doc, uids, origin, origin_seq) VALUES(?, ?, ?, ?) ON CONFLICT(doc)
		DO UPDATE SET uids = excluded.uids, origin = excluded.origin, origin_seq = excluded.origin_seq`, doc, string(b), o.origin, o.seq)
	return err
}

// docUIDs is a document's live uids: all of them, and the published ones (a content origin) in seq order.
type docUIDs struct {
	live map[string]bool
	pub  []string
}

func readDocUIDs(ctx context.Context, tx *sql.Tx, doc string) (docUIDs, error) {
	d := docUIDs{live: map[string]bool{}}
	rows, err := tx.QueryContext(ctx, `SELECT uid, origin <> '' FROM entries WHERE doc = ? AND deleted = 0
		AND uid IS NOT NULL ORDER BY seq`, doc)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var pub bool
		if err := rows.Scan(&uid, &pub); err != nil {
			return d, err
		}
		d.live[uid] = true
		if pub {
			d.pub = append(d.pub, uid)
		}
	}
	return d, rows.Err()
}

// orderChanged: a published insertion, or a relative reorder among published survivors; a delete alone is neither.
func orderChanged(before, after docUIDs) bool {
	if len(after.pub) == 0 {
		return false
	}
	var kept, now []string
	for _, u := range after.pub {
		if !before.live[u] {
			return true
		}
	}
	inAfter, inBefore := map[string]bool{}, map[string]bool{}
	for _, u := range after.pub {
		inAfter[u] = true
	}
	for _, u := range before.pub {
		inBefore[u] = true
		if inAfter[u] {
			kept = append(kept, u)
		}
	}
	for _, u := range after.pub {
		if inBefore[u] {
			now = append(now, u)
		}
	}
	return !slices.Equal(kept, now)
}

// captureDocOrder emits one order change for doc when a local write changed its published order; before is read first.
func captureDocOrder(ctx context.Context, tx *sql.Tx, doc string, before docUIDs) error {
	after, err := readDocUIDs(ctx, tx, doc)
	if err != nil || !orderChanged(before, after) {
		return err
	}
	cur, err := loadOrder(ctx, tx, doc)
	if err != nil {
		return err
	}
	return emitOrder(ctx, tx, doc, cur.base(), cur.uids, after.pub)
}

// emitOrder writes this machine's next order change for doc over base and makes uids the stamped state.
func emitOrder(ctx context.Context, tx *sql.Tx, doc, base string, baseUIDs, uids []string) error {
	e := memory.Envelope{Origin: machine(ctx, tx), Schema: memory.EnvelopeOrderSchema, DocRef: doc, BaseVersion: base,
		Op: memory.EnvelopeOpOrder, At: time.Now().UTC().Format(time.RFC3339)}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(origin_seq), 0) + 1 FROM changes WHERE origin = ?`, e.Origin).Scan(&e.Seq); err != nil {
		return err
	}
	var err error
	if e.Payload, err = json.Marshal(orderPayload{UIDs: slices.Concat([]string{}, uids), BaseUIDs: baseUIDs}); err != nil {
		return err
	}
	if err := insertEnvelopeChange(ctx, tx, e); err != nil {
		return err
	}
	return saveOrder(ctx, tx, doc, orderState{uids: uids, origin: e.Origin, seq: e.Seq})
}

// resolveOrder keeps one side of a document-order conflict and emits the kept array as this side's new order change.
func resolveOrder(ctx context.Context, tx *sql.Tx, id int64, keep string, e memory.Envelope) error {
	var older bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflicts WHERE op = 'order' AND doc = ? AND resolved = 0
		AND rowid < ?)`, e.DocRef, id).Scan(&older); err != nil {
		return err
	}
	if older {
		return fmt.Errorf("memory conflicts: resolve the older order conflict on %s first", e.DocRef)
	}
	var peer orderPayload
	if err := json.Unmarshal(e.Payload, &peer); err != nil {
		return err
	}
	cur, err := loadOrder(ctx, tx, e.DocRef)
	if err != nil {
		return err
	}
	if keep == "peer" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflicts(entry_rowid, entry_uid, peer_origin, peer_seq, payload, at, kind, op,
			doc, resolved) VALUES(NULL, NULL, ?, 0, ?, ?, 'replaced', 'order', ?, 1)`, machine(ctx, tx),
			string(mustJSON(orderPayload{UIDs: slices.Concat([]string{}, cur.uids)})), time.Now().UTC().Format(time.RFC3339), e.DocRef); err != nil {
			return err
		}
		if err := projectOrder(ctx, tx, e.DocRef, peer.UIDs); err != nil {
			return err
		}
		cur.uids = peer.UIDs
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conflicts SET resolved = 1 WHERE rowid = ?`, id); err != nil {
		return err
	}
	return emitOrder(ctx, tx, e.DocRef, baseOf(e.Origin, e.Seq), peer.UIDs, cur.uids)
}

type orderPayload struct {
	UIDs     []string `json:"uids"`
	BaseUIDs []string `json:"base_uids,omitempty"` // actual merged base array so successors can prove their base
}

func validOrder(e memory.Envelope) error {
	var p orderPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.UIDs == nil || e.EntryRef != "" {
		return fmt.Errorf("memory envelopes: %q/%d: an order change needs a uids payload and no entry ref", e.Origin, e.Seq)
	}
	for _, list := range [][]string{p.UIDs, p.BaseUIDs} {
		seen := map[string]bool{}
		for _, u := range list {
			if !entryUIDRE.MatchString(u) || seen[u] {
				return fmt.Errorf("memory envelopes: %q/%d: invalid or repeated uid %q in an order", e.Origin, e.Seq, u)
			}
			seen[u] = true
		}
	}
	return validDoc(e.DocRef)
}

// applyOrder takes a peer's order: same state stamps, a clean base or an append-only fork merges, else one conflict.
func applyOrder(ctx context.Context, tx *sql.Tx, e memory.Envelope) (bool, error) {
	var in orderPayload
	if err := json.Unmarshal(e.Payload, &in); err != nil {
		return false, err
	}
	cur, err := loadOrder(ctx, tx, e.DocRef)
	if err != nil {
		return false, err
	}
	peer := orderState{uids: in.UIDs, origin: e.Origin, seq: e.Seq}
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflicts WHERE op = 'order' AND doc = ? AND resolved = 0)`,
		e.DocRef).Scan(&open); err != nil {
		return false, err
	}
	switch {
	case slices.Equal(cur.uids, peer.uids):
		if cmpStamp(cur, peer) < 0 {
			cur = peer
		}
		if err := setOrder(ctx, tx, e.DocRef, cur); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE conflicts SET resolved = 1 WHERE op = 'order' AND doc = ?
			AND peer_origin = ? AND peer_seq < ? AND resolved = 0`, e.DocRef, e.Origin, e.Seq)
		return false, err
	case open:
		return true, insertOrderConflict(ctx, tx, e, conflictBlocked)
	case e.BaseVersion == cur.base():
		// A merged array has no envelope of its own; its stamp alone must not let one fork overwrite the other.
		base, held, err := orderBase(ctx, tx, e.DocRef, cur.base())
		if err != nil {
			return false, err
		}
		if cur.origin == "" || in.BaseUIDs != nil && slices.Equal(in.BaseUIDs, cur.uids) ||
			in.BaseUIDs == nil && held && slices.Equal(base, cur.uids) {
			return false, setOrder(ctx, tx, e.DocRef, peer)
		}
	}
	merged, ok, err := mergeAppends(ctx, tx, e, cur, peer)
	if err != nil || !ok {
		if err == nil {
			err = insertOrderConflict(ctx, tx, e, conflictDiverged)
		}
		return !ok, err
	}
	return false, setOrder(ctx, tx, e.DocRef, merged)
}

// setOrder stores next and rewrites the document's seq to match.
func setOrder(ctx context.Context, tx *sql.Tx, doc string, next orderState) error {
	if err := saveOrder(ctx, tx, doc, next); err != nil {
		return err
	}
	return projectOrder(ctx, tx, doc, next.uids)
}

// orderBase is the order array of the change base names; "" is the empty base, ok false when it is not held.
func orderBase(ctx context.Context, tx *sql.Tx, doc, base string) (uids []string, ok bool, err error) {
	if base == "" {
		return nil, true, nil
	}
	origin, seq, found := strings.Cut(base, "/")
	n, perr := strconv.ParseInt(seq, 10, 64)
	if !found || perr != nil {
		return nil, false, nil
	}
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(payload, '') FROM changes WHERE origin = ? AND origin_seq = ? AND op = 'order'
		AND doc_ref = ?`, origin, n, doc).Scan(&raw)
	var p orderPayload
	if errors.Is(err, sql.ErrNoRows) || err == nil && json.Unmarshal([]byte(raw), &p) != nil {
		return nil, false, nil
	}
	return p.UIDs, err == nil, err
}

func hasPrefix(s, prefix []string) bool {
	return len(s) >= len(prefix) && slices.Equal(s[:len(prefix)], prefix)
}

// mergeAppends joins two forks that only appended to the shared base order, appended uids in stamp order.
func mergeAppends(ctx context.Context, tx *sql.Tx, e memory.Envelope, cur, peer orderState) (orderState, bool, error) {
	base, ok, err := orderBase(ctx, tx, e.DocRef, e.BaseVersion)
	storedBase := base
	var incoming orderPayload
	if err := json.Unmarshal(e.Payload, &incoming); err != nil {
		return orderState{}, false, err
	}
	if incoming.BaseUIDs != nil {
		base, ok = incoming.BaseUIDs, true
	}
	if err != nil || !ok || !hasPrefix(cur.uids, base) || !hasPrefix(peer.uids, base) {
		return orderState{}, false, err
	}
	parts, err := siblingAppends(ctx, tx, e, base, storedBase, cur.uids)
	if err != nil {
		return orderState{}, false, err
	}
	prior, ok := joinAppends(base, parts)
	if !ok || len(parts) == 0 || !slices.Equal(prior, cur.uids) || cmpStamp(parts[len(parts)-1], cur) != 0 {
		return orderState{}, false, nil
	}
	parts = canonicalOrders(append(parts, peer))
	merged, ok := joinAppends(base, parts)
	if !ok {
		return orderState{}, false, nil
	}
	last := parts[len(parts)-1]
	return orderState{uids: merged, origin: last.origin, seq: last.seq}, true, nil
}

// siblingAppends loads same-base order branches already represented in cur, deduplicated in stamp order.
func siblingAppends(ctx context.Context, tx *sql.Tx, e memory.Envelope, base, storedBase, cur []string) ([]orderState, error) {
	rows, err := tx.QueryContext(ctx, `SELECT origin, origin_seq, payload FROM changes
		WHERE op = 'order' AND doc_ref = ? AND COALESCE(base_version, '') = ?`, e.DocRef, e.BaseVersion)
	if err != nil {
		return nil, err
	}
	var parts []orderState
	for rows.Next() {
		var part orderState
		var raw string
		if err := rows.Scan(&part.origin, &part.seq, &raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if keepSibling(&part, raw, base, storedBase, cur) {
			parts = append(parts, part)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return canonicalOrders(parts), nil
}

func keepSibling(part *orderState, raw string, base, storedBase, cur []string) bool {
	var p orderPayload
	if json.Unmarshal([]byte(raw), &p) != nil || !hasPrefix(p.UIDs, base) {
		return false
	}
	partBase := p.BaseUIDs
	if partBase == nil {
		partBase = storedBase
	}
	if !slices.Equal(partBase, base) {
		return false
	}
	part.uids = p.UIDs
	return !slices.ContainsFunc(part.uids[len(base):], func(u string) bool { return !slices.Contains(cur, u) })
}

func canonicalOrders(parts []orderState) []orderState {
	slices.SortFunc(parts, cmpStamp)
	var out []orderState
	for i, part := range parts {
		if !slices.ContainsFunc(parts[i+1:], func(other orderState) bool { return slices.Equal(part.uids, other.uids) }) {
			out = append(out, part)
		}
	}
	return out
}

func joinAppends(base []string, parts []orderState) ([]string, bool) {
	out := slices.Clone(base)
	for _, part := range parts {
		for _, u := range part.uids[len(base):] {
			if slices.Contains(out, u) { // overlapping suffixes are not independent appends
				return nil, false
			}
			out = append(out, u)
		}
	}
	return out, true
}

// projectOrder gives doc's live entries the listed order first, the rest after in their current order; only seq moves.
func projectOrder(ctx context.Context, tx *sql.Tx, doc string, uids []string) error {
	d, err := readDocRows(ctx, tx, doc)
	if err != nil {
		return err
	}
	var order []int64
	for _, u := range uids {
		if r, ok := d.byUID[u]; ok {
			order = append(order, r)
		}
	}
	for _, r := range d.rowids {
		if !slices.Contains(order, r) {
			order = append(order, r)
		}
	}
	if err := parkDocRows(ctx, tx, doc); err != nil {
		return err
	}
	for i, r := range order {
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = ? WHERE rowid = ?`, i, r); err != nil {
			return err
		}
	}
	return nil
}

// parkDocRows moves doc's rows below any existing negative seq first to avoid transient UNIQUE collisions.
func parkDocRows(ctx context.Context, tx *sql.Tx, doc string) error {
	var floor int64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(COALESCE(MIN(seq), 0), -COALESCE(MAX(rowid), 0))
		FROM entries WHERE doc = ?`, doc).Scan(&floor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = ? - rowid WHERE doc = ?`, floor, doc); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE entries SET seq = -rowid WHERE doc = ?`, doc)
	return err
}

type docRows struct {
	rowids []int64
	byUID  map[string]int64
}

func readDocRows(ctx context.Context, tx *sql.Tx, doc string) (docRows, error) {
	d := docRows{byUID: map[string]int64{}}
	rows, err := tx.QueryContext(ctx, `SELECT rowid, uid FROM entries WHERE doc = ? AND deleted = 0 ORDER BY seq`, doc)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var r int64
		var uid sql.NullString
		if err := rows.Scan(&r, &uid); err != nil {
			return d, err
		}
		d.rowids = append(d.rowids, r)
		if uid.Valid {
			d.byUID[uid.String] = r
		}
	}
	return d, rows.Err()
}

// insertOrderConflict files each distinct order variant once, at document scope (no entry uid, NULL rowid).
func insertOrderConflict(ctx context.Context, tx *sql.Tx, e memory.Envelope, kind string) error {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(payload, '') FROM conflicts WHERE peer_origin = ?
		AND peer_seq = ? AND op = 'order' AND doc = ? AND COALESCE(base_version, '') = ?`,
		e.Origin, e.Seq, e.DocRef, e.BaseVersion)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		if samePayload(raw, e.Payload) {
			return rows.Close()
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conflicts(entry_rowid, entry_uid, peer_origin, peer_seq, payload, at, kind, op,
		base_version, doc) VALUES(NULL, NULL, ?, ?, ?, ?, ?, 'order', ?, ?)`,
		e.Origin, e.Seq, string(e.Payload), e.At, kind, e.BaseVersion, e.DocRef)
	return err
}

// exportAfterWrite brings the Markdown up to date after a committed envelope write, under the held bank lock.
func exportAfterWrite() error {
	edb, closeDB, err := openExportDB()
	if err != nil {
		return err
	}
	_, err = memory.ExportGuardLocked(edb, paths.MemoryStoreDir())
	return errors.Join(err, closeDB())
}
