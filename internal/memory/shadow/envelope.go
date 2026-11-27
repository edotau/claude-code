package shadow

import (
	"context"
	"database/sql"
)

const machineQ = `(SELECT value FROM meta WHERE key = 'machine_id')`

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

// dropCaptureV7 removes the change-capture triggers: nothing reads the changes they wrote; their tables stay.
func dropCaptureV7(tx *sql.Tx) (string, error) {
	for _, q := range []string{`DROP TRIGGER IF EXISTS entries_capture_ai`, `DROP TRIGGER IF EXISTS entries_capture_au`} {
		if _, err := tx.ExecContext(context.Background(), q); err != nil {
			return "", err
		}
	}
	return "drop change capture triggers", nil
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

// dropSyncV8 drops the peer-sync tables, peer conflict rows and entry columns only sync read; export conflicts stay.
func dropSyncV8(tx *sql.Tx) (string, error) {
	for _, q := range []string{`DROP TABLE IF EXISTS changes`, `DROP TABLE IF EXISTS peers`,
		`DROP TABLE IF EXISTS doc_orders`, `DROP TABLE IF EXISTS tombstones`, `DELETE FROM conflicts WHERE entry_rowid <> 0`, `DELETE FROM meta WHERE key = 'machine_id'`,
		`ALTER TABLE entries DROP COLUMN origin`, `ALTER TABLE entries DROP COLUMN origin_seq`,
		`ALTER TABLE entries DROP COLUMN rendered_rev`} {
		if _, err := tx.ExecContext(context.Background(), q); err != nil {
			return "", err
		}
	}
	return "drop sync tables", nil
}
