package auditq

import (
	"context"
	"database/sql"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// steps is the audit store's migration list; step n sets user_version n.
var steps = []sqlite.Step{{Name: "audits", Up: auditsV1}}

// auditsV1 is IF NOT EXISTS because Migrate reads the version outside its transaction: two workers may both apply it.
const auditsV1SQL = `CREATE TABLE IF NOT EXISTS audits(
	sha TEXT NOT NULL, queued INTEGER NOT NULL, -- unix ns: with sha, the enqueue token
	repo TEXT NOT NULL, base TEXT NOT NULL,
	state TEXT NOT NULL CHECK(state IN ('queued', 'running', 'done')),
	started INTEGER NOT NULL DEFAULT 0, -- unix s: the covering run's start
	ended INTEGER NOT NULL DEFAULT 0, -- unix s: when the row turned done
	log TEXT NOT NULL DEFAULT '', with_shas TEXT NOT NULL DEFAULT '',
	code INTEGER NOT NULL DEFAULT 0, secs INTEGER NOT NULL DEFAULT 0,
	slot TEXT NOT NULL DEFAULT '', run TEXT NOT NULL DEFAULT '', -- checkout and tree tag: a dead worker's row
	-- requeues once no process is in the slot and none carries the tag
	PRIMARY KEY(sha, queued))`

func auditsV1(tx *sql.Tx) (string, error) {
	_, err := tx.ExecContext(context.Background(), auditsV1SQL)
	return "audits table", err
}

const (
	openRetries = 20
	openBackoff = 50 * time.Millisecond
	busyWait    = 5 * time.Second
)

// open opens and migrates the store; the queue is rebuilt by the next land, so no snapshot is kept.
func open(ctx context.Context) (*sql.DB, error) {
	db, err := sqlite.Open(paths.AuditDBPath(), sqlite.Options{Busy: busyWait})
	for i := 0; err != nil && i < openRetries; i++ { // two first opens race on the WAL switch, which never waits
		time.Sleep(openBackoff)
		db, err = sqlite.Open(paths.AuditDBPath(), sqlite.Options{Busy: busyWait})
	}
	if err != nil {
		return nil, err
	}
	if _, err = sqlite.Migrate(ctx, db, steps, sqlite.MigrateOptions{Name: "audit"}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
