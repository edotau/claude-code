package shadow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/sqlite"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func init() { memory.OpenVectorStore = func() memory.VectorStore { return sqlVectors{} } }

// vectorsStmts hold the embedding record; the entry-keyed `vectors` table is emptied by rebuilds, so these are apart.
var vectorsStmts = []string{
	`CREATE TABLE embed_meta(id INTEGER PRIMARY KEY CHECK(id = 1), schema INTEGER, model TEXT, dim INTEGER,
		dim_at INTEGER, minted_at INTEGER)`,
	`CREATE TABLE embed_rows(id TEXT PRIMARY KEY, root TEXT, hash TEXT, block_hash TEXT, vec BLOB)`,
	`CREATE TABLE embed_claim(id INTEGER PRIMARY KEY CHECK(id = 1), expiry INTEGER, owner INTEGER)`,
}

func vectorsV4(tx *sql.Tx) (string, error) {
	for _, s := range vectorsStmts {
		if _, err := tx.ExecContext(context.Background(), s); err != nil {
			return "", fmt.Errorf("memory: shadow vectors: %w", err)
		}
	}
	return "3 tables", nil
}

const metaVectorsImported = "embed_import"

// sqlVectors is memory.VectorStore over the shadow db; it holds no state, each call opens the db.
type sqlVectors struct{}

// open returns nil, nil when no shadow db and no legacy file exist: nothing to read, nothing to create.
func (sqlVectors) open(create bool) (*sql.DB, error) {
	_, serr := os.Stat(ShadowDBPath())
	if errors.Is(serr, fs.ErrNotExist) && !create {
		if _, lerr := os.Stat(memory.EmbedDBPath()); lerr != nil {
			return nil, nil
		}
	}
	ctx := context.Background()
	db, err := openMigratedShadow(ctx)
	if err != nil {
		return nil, err
	}
	if err := importLegacyVectors(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (sqlVectors) Present() bool {
	for _, p := range []string{ShadowDBPath(), memory.EmbedDBPath()} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// importLegacyVectors copies embeddings.db in once when the tables are empty; the file is kept, never deleted.
func importLegacyVectors(ctx context.Context, db *sql.DB) error {
	if _, err := os.Stat(memory.EmbedDBPath()); err != nil {
		return nil
	}
	var done int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key = ?`, metaVectorsImported).Scan(&done); err != nil || done > 0 {
		return err
	}
	rec, err := readLegacyVectors() // a damaged cache is derived data: it imports as empty, once
	if err != nil {
		return nil // busy or unreadable by permission: try again at the next open, nothing is marked
	}
	return sqlite.Tx(ctx, db, func(tx *sql.Tx) error { return importTx(ctx, tx, rec) })
}

// importTx rechecks inside the transaction that no peer imported first, then stores rec and the done marker.
func importTx(ctx context.Context, tx *sql.Tx, rec memory.VectorRecord) error {
	var marked, stored int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE key = ?`, metaVectorsImported).Scan(&marked); err != nil || marked > 0 {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM embed_meta`).Scan(&stored); err != nil {
		return err
	}
	if stored == 0 && rec.Model != "" {
		if err := writeRecord(ctx, tx, rec); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)`, metaVectorsImported, time.Now().UTC().Format(time.RFC3339))
	return err
}

var (
	legacyVectorBucket = []byte("vectors")
	legacyRowsBucket   = []byte("rows")
	legacyMetaKey      = []byte("metadata")
)

// readLegacyVectors decodes embeddings.db read-only; any failure or schema mismatch is the empty record.
func readLegacyVectors() (memory.VectorRecord, error) {
	db, err := bbolt.Open(memory.EmbedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) || errors.Is(err, fs.ErrPermission) {
		return memory.VectorRecord{}, err
	}
	if err != nil {
		return memory.VectorRecord{}, nil
	}
	defer db.Close()
	var want int64
	_ = db.View(func(tx *bbolt.Tx) error { want = tx.Size(); return nil })
	if st, err := os.Stat(memory.EmbedDBPath()); err != nil || st.Size() < want { // bbolt faults on a cut-short file
		return memory.VectorRecord{}, nil
	}
	var rec memory.VectorRecord
	_ = db.View(func(tx *bbolt.Tx) error {
		rec = decodeLegacy(tx.Bucket(legacyVectorBucket))
		return nil
	})
	return rec, nil
}

func decodeLegacy(b *bbolt.Bucket) memory.VectorRecord {
	if b == nil {
		return memory.VectorRecord{}
	}
	var rec memory.VectorRecord
	if !decodeMeta(b.Get(legacyMetaKey), &rec) || rec.Schema != memory.EmbedCacheSchema || rec.Rows == nil {
		return memory.VectorRecord{}
	}
	rows := b.Bucket(legacyRowsBucket)
	if rows == nil {
		return memory.VectorRecord{}
	}
	for id, row := range rec.Rows {
		raw := rows.Get([]byte(id))
		if raw == nil || len(raw)%4 != 0 {
			delete(rec.Rows, id)
			continue
		}
		row.Vec = string(raw)
		rec.Rows[id] = row
	}
	return rec
}

// writeRecord replaces the stored record with rec, stamped with the current schema.
func writeRecord(ctx context.Context, tx *sql.Tx, rec memory.VectorRecord) error {
	for _, q := range []string{`DELETE FROM embed_rows`, `DELETE FROM embed_meta`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO embed_meta(id, schema, model, dim, dim_at, minted_at) VALUES(1, ?, ?, ?, ?, ?)`,
		memory.EmbedCacheSchema, rec.Model, rec.Dim, rec.DimAt, rec.MintedAt); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO embed_rows(id, root, hash, block_hash, vec) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, r := range rec.Rows {
		if _, err := stmt.ExecContext(ctx, id, r.Root, r.Hash, r.BlockHash, []byte(r.Vec)); err != nil {
			return err
		}
	}
	return nil
}

// readHead reads the meta row; ok is false when none is stored or its schema is not the current one.
func readHead(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (rec memory.VectorRecord, ok bool, err error) {
	var schema int
	err = q.QueryRowContext(ctx, `SELECT schema, model, dim, dim_at, minted_at FROM embed_meta WHERE id = 1`).
		Scan(&schema, &rec.Model, &rec.Dim, &rec.DimAt, &rec.MintedAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && schema != memory.EmbedCacheSchema {
		return memory.VectorRecord{}, false, nil
	}
	rec.Schema = schema
	return rec, err == nil, err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRecord(ctx context.Context, q queryer) (memory.VectorRecord, error) {
	rec, ok, err := readHead(ctx, q)
	if err != nil || !ok {
		return memory.VectorRecord{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, root, hash, block_hash, vec FROM embed_rows`)
	if err != nil {
		return memory.VectorRecord{}, err
	}
	rec.Rows = map[string]memory.VectorRow{}
	for rows.Next() {
		var id string
		var r memory.VectorRow
		var vec []byte
		if err := rows.Scan(&id, &r.Root, &r.Hash, &r.BlockHash, &vec); err != nil {
			_ = rows.Close()
			return memory.VectorRecord{}, err
		}
		if len(vec)%4 != 0 {
			continue
		}
		r.Vec = string(vec)
		rec.Rows[id] = r
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return memory.VectorRecord{}, err
	}
	return rec, nil
}

func (s sqlVectors) Vectors() (memory.VectorRecord, error) {
	db, err := s.open(false)
	if db == nil || err != nil {
		return memory.VectorRecord{}, err
	}
	defer db.Close()
	return readRecord(context.Background(), db)
}

func (s sqlVectors) Head() (memory.VectorHead, error) {
	db, err := s.open(false)
	if db == nil || err != nil {
		return memory.VectorHead{}, err
	}
	defer db.Close()
	ctx := context.Background()
	rec, ok, err := readHead(ctx, db)
	if err != nil || !ok {
		return memory.VectorHead{}, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM embed_rows`).Scan(&n); err != nil {
		return memory.VectorHead{}, err
	}
	return memory.VectorHead{Model: rec.Model, MintedAt: rec.MintedAt, Rows: n > 0}, nil
}

func (s sqlVectors) Claim(owner int64, now, expiry time.Time) (held time.Time, ok bool, err error) {
	db, err := s.open(true)
	if err != nil {
		return time.Time{}, false, err
	}
	defer db.Close()
	ctx := context.Background()
	err = sqlite.Tx(ctx, db, func(tx *sql.Tx) error {
		held, ok = time.Time{}, false
		var exp int64
		qerr := tx.QueryRowContext(ctx, `SELECT expiry FROM embed_claim WHERE id = 1`).Scan(&exp)
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			return qerr
		}
		if qerr == nil && time.Unix(0, exp).After(now) {
			held = time.Unix(0, exp)
			return nil
		}
		ok = true
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO embed_claim(id, expiry, owner) VALUES(1, ?, ?)`, expiry.UnixNano(), owner)
		return err
	})
	return held, ok && err == nil, err
}

func claimOwnedBy(ctx context.Context, tx *sql.Tx, owner int64) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM embed_claim WHERE id = 1 AND owner = ?`, owner).Scan(&n)
	return n > 0, err
}

func (s sqlVectors) Release(owner int64) error {
	db, err := s.open(true)
	if err != nil {
		return err
	}
	defer db.Close()
	return sqlite.Tx(context.Background(), db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `DELETE FROM embed_claim WHERE id = 1 AND owner = ?`, owner)
		return err
	})
}

func (s sqlVectors) Put(w memory.VectorWrite) (stored bool, err error) {
	db, err := s.open(true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx := context.Background()
	err = sqlite.Tx(ctx, db, func(tx *sql.Tx) error {
		stored = false
		if owned, err := claimOwnedBy(ctx, tx, w.Owner); err != nil || !owned { // taken over or expired: refuse
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM embed_claim WHERE id = 1`); err != nil {
			return err
		}
		cur, err := readRecord(ctx, tx)
		if err != nil {
			return err
		}
		if cur.MintedAt >= w.Started && cur.MintedAt <= time.Now().UnixNano() {
			return nil
		}
		stored = true
		return writeRecord(ctx, tx, w.Merge(cur))
	})
	return stored && err == nil, err
}

// RefreshDue is due when no store exists or the record is outside its lease and no claim is live.
func (s sqlVectors) RefreshDue(now time.Time) bool {
	db, err := s.open(false)
	if err != nil {
		return false
	}
	if db == nil {
		return true
	}
	defer db.Close()
	ctx := context.Background()
	rec, _, err := readHead(ctx, db)
	if err != nil {
		return false
	}
	if memory.EmbedValid(rec, now) {
		return false
	}
	var exp int64
	qerr := db.QueryRowContext(ctx, `SELECT expiry FROM embed_claim WHERE id = 1`).Scan(&exp)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return false
	}
	return qerr != nil || !time.Unix(0, exp).After(now)
}

func decodeMeta(raw []byte, rec *memory.VectorRecord) bool {
	return len(raw) > 0 && json.Unmarshal(raw, rec) == nil
}
