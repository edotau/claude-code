package vault

import (
	"errors"
	"io"
	"os"
	"strconv"

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

// SnapshotTo writes a consistent, point-in-time copy of the vault at path to w via one read
// transaction, so an archiver (nuke) never captures bytes straddling an in-flight write (R8).
// It opens its own bbolt.DB directly rather than going through View, so it runs alongside an
// unrelated writer without the architectural contract that View/Update callbacks must not nest.
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
