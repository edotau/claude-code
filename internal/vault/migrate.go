package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Schema is the layout number stamped in meta/schema (0 when unset).
func (t *Tx) Schema() int {
	s, _ := t.Get(Meta, "schema")
	n, _ := strconv.Atoi(s)
	return n
}

// SetSchema stamps meta/schema.
func (t *Tx) SetSchema(n int) error { return t.Put(Meta, "schema", strconv.Itoa(n)) }

// OpenSecret decrypts name inside t with k; found is false for an absent record.
func (t *Tx) OpenSecret(k *Key, name string) (value string, rec Record, found bool, err error) {
	rec, found, err = t.Record(name)
	if err != nil || !found {
		return "", Record{}, false, err
	}
	value, err = k.Open(name, rec)
	if err != nil {
		return "", Record{}, false, fmt.Errorf("vault: open %q: %w", name, err)
	}
	return value, rec, true, nil
}

// SealSecret seals value under name with k, keeping from's display metadata (prefix, set/verify stamps); Len is recomputed.
func (t *Tx) SealSecret(k *Key, name, value string, from Record) error {
	from.Sealed, from.Kid, from.Len = k.Seal(name, value), k.Kid, len(value)
	return t.PutRecord(name, from)
}

// RenameSecret moves name's value to newName inside t, re-sealed under newName (the name is bound into the AEAD AAD).
func (t *Tx) RenameSecret(k *Key, name, newName string) error {
	v, rec, ok, err := t.OpenSecret(k, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("vault: secret %q not found", name)
	}
	if err := t.SealSecret(k, newName, v, rec); err != nil {
		return err
	}
	if name == newName {
		return nil
	}
	return t.Put(Secrets, name, "")
}

// BackupPath is the vault's backup copy for time at.
func BackupPath(at time.Time) string { return Path() + ".bak-" + at.UTC().Format("20060102T150405Z") }

// Backup copies the vault to vault.db.bak-<UTC timestamp> (0600) through a consistent read transaction.
func Backup() (string, error) {
	path := BackupPath(time.Now())
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	if err := SnapshotTo(Path(), f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
