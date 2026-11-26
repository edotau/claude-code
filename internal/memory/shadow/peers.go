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
		_, err := tx.ExecContext(ctx, `INSERT INTO peers(origin, acked_seq, schema) SELECT origin, MAX(origin_seq), ?
			FROM changes WHERE origin = ? GROUP BY origin ON CONFLICT(origin) DO UPDATE SET
			acked_seq = MAX(acked_seq, excluded.acked_seq)`, memory.EnvelopeSchema, old)
		return err
	})
	return id, errors.Join(err, db.Close())
}

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
	return memory.ImportEnvelopes(to, &buf, memory.EnvelopeSchema)
}
