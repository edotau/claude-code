package vault

import (
	"errors"
	"os"
	"strconv"

	"go.etcd.io/bbolt"

	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "vault", Owner: "vault", Kind: schemas.Store, Glob: Path, Version: schemaVersion,
	Keep: "encrypted vault; the user's secrets", Probe: probeSchema,
})

// probeSchema reads meta/schema straight from the bbolt file; no master key, no checkSchema refusal (0 when absent).
func probeSchema() (int, error) {
	db, err := bbolt.Open(Path(), 0600, &bbolt.Options{ReadOnly: true, Timeout: readLockTimeout})
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
			n, _ = strconv.Atoi(string(b.Get([]byte("schema"))))
		}
		return nil
	})
	return n, err
}
