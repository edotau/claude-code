package shadow

import (
	"context"
	"database/sql"

	"github.com/edotau/claude-code/internal/memory"
)

func (s sqlEnvelopeStore) LocalSchema() int { return s.schema }

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
	return ackedSeq(t.ctx, t.tx, origin)
}

func (t txEnvelopeStore) ApplyEnvelope(e memory.Envelope) (bool, error) {
	if err := checkApply(e); err != nil {
		return false, err
	}
	return applyEnvelopeTx(t.ctx, t.tx, e)
}
