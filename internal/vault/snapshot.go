package vault

import (
	"errors"
	"io"
	"io/fs"
	"time"

	"go.etcd.io/bbolt"
)

// SnapshotTo writes a consistent, point-in-time copy of the vault at path to w via one read
// transaction, so an archiver (nuke) never captures bytes straddling an in-flight write (R8).
// It opens its own bbolt.DB directly rather than going through View, so it runs alongside an
// unrelated writer without the architectural contract that View/Update callbacks must not nest.
func SnapshotTo(path string, w io.Writer) error {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(func(tx *bbolt.Tx) error {
		_, err := tx.WriteTo(w)
		return err
	})
}
