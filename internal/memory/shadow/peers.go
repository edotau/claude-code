package shadow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/sqlite"
)

// mintMachineID gives the copy at path a fresh machine_id and acks the old id's history, so it is never replayed.
func mintMachineID(ctx context.Context, path string) (string, error) {
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return "", errors.Join(errors.New("memory bootstrap: open copy"), err)
	}
	var id string
	err = sqlite.Tx(ctx, db, func(tx *sql.Tx) error {
		old := machine(ctx, tx)
		if old == "" {
			return errors.New("memory bootstrap: source has no machine_id: not a migrated memory db")
		}
		if err := tx.QueryRowContext(ctx, `SELECT lower(hex(randomblob(16)))`).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'machine_id'`, id); err != nil {
			return err
		}
		// The snapshot is authoritative even when the source has never imported Markdown.
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, metaImported, importMarker()); err != nil {
			return err
		}
		// The source's file fingerprints describe its files, not the new store's; export writes and restamps them.
		if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key LIKE 'render:%' OR key LIKE 'stat:%'`); err != nil {
			return err
		}
		// The source's embed claim names its owner and expiry; the new machine must not inherit it.
		var has int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'embed_claim'`).Scan(&has); err != nil {
			return err
		}
		if has > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM embed_claim`); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO peers(origin, acked_seq, schema) SELECT origin, MAX(origin_seq),
			MAX(COALESCE(schema, 0), ?) FROM changes WHERE origin = ? GROUP BY origin ON CONFLICT(origin) DO UPDATE SET
			acked_seq = MAX(acked_seq, excluded.acked_seq), schema = MAX(peers.schema, excluded.schema)`, memory.EnvelopeSchema, old)
		return err
	})
	return id, errors.Join(err, db.Close())
}

func (s sqlEnvelopeStore) LocalSchema() int { return s.schema }

// SyncPair sends each origin's changes after to's watermark; the importer safely acknowledges echoed own changes.
func SyncPair(ctx context.Context, from, to memory.EnvelopeStore, origins []string) (memory.ImportReport, error) {
	if err := ctx.Err(); err != nil {
		return memory.ImportReport{}, err
	}
	var buf bytes.Buffer
	for _, o := range slices.Compact(slices.Sorted(slices.Values(origins))) {
		if err := ctx.Err(); err != nil {
			return memory.ImportReport{}, err
		}
		after, _, err := to.Watermark(o)
		if err != nil {
			return memory.ImportReport{}, fmt.Errorf("memory sync: watermark %q: %w", o, err)
		}
		if _, err := memory.ExportEnvelopes(from, o, after, &buf); err != nil {
			return memory.ImportReport{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return memory.ImportReport{}, err
	}
	return memory.ImportEnvelopes(to, &buf, memory.EnvelopeMaxSchema)
}

// Origins is this db's machine_id and every origin it holds changes or a watermark for.
func (s sqlEnvelopeStore) Origins() (string, []string, error) {
	ctx := context.Background()
	var self string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM meta WHERE key = 'machine_id'), '')`).Scan(&self); err != nil {
		return "", nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT origin FROM changes UNION SELECT origin FROM peers ORDER BY 1`)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var all []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return "", nil, err
		}
		all = append(all, o)
	}
	return self, all, rows.Err()
}

// Batch runs f over a store whose applies share one transaction and one bank-lock hold: any error rolls all back.
func (s sqlEnvelopeStore) Batch(f func(memory.EnvelopeStore) error) error {
	return s.write(func(ctx context.Context, tx *sql.Tx) error {
		return f(txEnvelopeStore{sqlEnvelopeStore: s, ctx: ctx, tx: tx})
	})
}

// txEnvelopeStore reads watermarks and applies inside the Batch transaction; its other methods are the plain store's.
type txEnvelopeStore struct {
	sqlEnvelopeStore
	ctx context.Context
	tx  *sql.Tx
}

func (t txEnvelopeStore) Watermark(origin string) (int64, int, error) {
	var seq int64
	var schema int
	err := t.tx.QueryRowContext(t.ctx, `SELECT acked_seq, schema FROM peers WHERE origin = ?`, origin).Scan(&seq, &schema)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return seq, schema, err
}

func (t txEnvelopeStore) ApplyEnvelope(e memory.Envelope) (bool, error) {
	if err := checkApply(e); err != nil {
		return false, err
	}
	return applyEnvelopeTx(t.ctx, t.tx, e)
}
