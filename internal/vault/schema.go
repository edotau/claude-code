package vault

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"go.etcd.io/bbolt"
)

var _ = schemas.Register(schemas.Format{
	Name: "vault", Owner: "vault", Kind: schemas.Store, Glob: Path, Version: schemaVersion,
	Keep: "encrypted vault; the user's secrets", Probe: probeSchema,
})

// probeSchema reads meta/schema straight from the bbolt file; no master key, no checkSchema refusal (0 when absent).
func probeSchema() (int, error) {
	db, err := openReadOnly(Path())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer db.Close()
	n := 0
	err = db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(Meta)); b != nil {
			n = parseSchema(string(b.Get([]byte("schema"))))
		}
		return nil
	})
	return n, err
}

// Schema is the layout number stamped in meta/schema (0 when unset).
func (t *Tx) Schema() int {
	s, _ := t.Get(Meta, "schema")
	return parseSchema(s)
}

// parseSchema reads a stamped layout number; unset or malformed is 0.
func parseSchema(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// openReadOnly opens the bbolt file at path read-only, bounded by readLockTimeout.
func openReadOnly(path string) (*bbolt.DB, error) {
	return bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: readLockTimeout})
}

// SetSchema stamps meta/schema.
func (t *Tx) SetSchema(n int) error { return t.Put(Meta, "schema", strconv.Itoa(n)) }

// SnapshotTo copies the vault at path to w in one read txn (R8); its own bbolt.DB, not View, so it never nests.
func SnapshotTo(path string, w io.Writer) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(func(tx *bbolt.Tx) error {
		_, err := tx.WriteTo(w)
		return err
	})
}

// snapshotBeforeMigration copies an existing, older vault to paths.BackupPath before its first migrating write.
func snapshotBeforeMigration(db *bbolt.DB, path string) error {
	var buf bytes.Buffer
	err := db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(Meta)); b != nil && parseSchema(string(b.Get([]byte("schema")))) >= schemaVersion {
			return nil
		}
		if tx.Bucket([]byte(Pins)) == nil {
			return nil
		}
		_, err := tx.WriteTo(&buf)
		return err
	})
	if err != nil || buf.Len() == 0 {
		return err
	}
	return paths.AtomicWrite(paths.BackupPath(path, time.Now()), buf.Bytes(), 0600)
}

// reportAgentsOff prints one line per agent the migration left without a profile.
func reportAgentsOff(off []string) {
	for _, a := range off {
		fmt.Fprintf(os.Stderr, "vault: %s left off (no profile); pick one in `claude-code configure` → \"Seed agent profiles\"\n", a)
	}
}
