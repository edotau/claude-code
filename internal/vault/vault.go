// Package vault is the encrypted bbolt store behind pins, custom env, provider/agent overlays and sealed secrets.
package vault

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

const (
	Pins            = "pins"
	Env             = "env"
	Overlay         = "overlay"
	Secrets         = "secrets"
	SettingsBackups = "settings_backups"
	Meta            = "meta"
	// MemoryOrigins maps a memory store key to its JSON origin record; plaintext, readable without the master key.
	MemoryOrigins = "memory_origins"
)

var (
	ErrLocked   = errors.New("vault: no master key (keyring, HARNESS_VAULT_KEY or key_command)")
	ErrWrongKey = errors.New("vault: master key does not match this vault")
)

// schemaVersion is the one vault layout this build reads; the upgrades from older layouts live in git history.
const schemaVersion = 7

// Lock-wait bounds for bbolt opens; vars so tests can shorten them.
var (
	readLockTimeout  = 2 * time.Second
	writeLockTimeout = 5 * time.Second
)

// SetReadLockTimeoutForTest overrides the bbolt read-lock timeout and returns a restore func; test-only.
func SetReadLockTimeoutForTest(d time.Duration) (restore func()) {
	saved := readLockTimeout
	readLockTimeout = d
	return func() { readLockTimeout = saved }
}

func Path() string { return filepath.Join(paths.ConfigDir(), "state", "vault.db") }

// View and Update are transaction BOUNDARIES: never call View, Update, UpdateSealed or MasterKey (it opens a View)
// from inside another's callback; bbolt's file lock is not reentrant, so a nested open blocks until bbolt.ErrTimeout.
// Helpers that must run in an open transaction take the *Tx as a parameter; no runtime guard enforces this.
func View(fn func(*Tx) error) error { return ViewAt(Path(), fn) }

func ViewAt(path string, fn func(*Tx) error) error {
	db, err := openReadOnly(path)
	if errors.Is(err, os.ErrNotExist) {
		return fn(&Tx{})
	}
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(func(tx *bbolt.Tx) error {
		t := &Tx{tx}
		if err := checkSchema(t); err != nil {
			return err
		}
		return fn(t)
	})
}

func checkSchema(t *Tx) error {
	s, _ := t.Get(Meta, "schema")
	n, err := strconv.Atoi(s)
	if s != "" && err != nil {
		return fmt.Errorf("vault: invalid schema %q", s)
	}
	if n > schemaVersion {
		return fmt.Errorf("vault: written by a newer claude-code (schema %d)", n)
	}
	if n < schemaVersion {
		return fmt.Errorf("vault: schema %d is older than this build reads (schema %d); open it once with a "+
			"claude-code built before the vault migrations were removed (git history), or restore state/vault.db "+
			"from a backup", n, schemaVersion)
	}
	return nil
}

func create(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "vault.db.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // best-effort cleanup; os.Link below moves it into place on success
	if err = f.Close(); err != nil {
		return err
	}
	if err = initVaultFile(tmp); err != nil {
		return err
	}
	if err = os.Link(tmp, path); errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

// initVaultFile seeds a fresh bbolt file at tmp with every bucket, the schema stamp and a new vault_id.
func initVaultFile(tmp string) error {
	db, err := bbolt.Open(tmp, 0600, &bbolt.Options{NoSync: noSync.Load()})
	if err != nil {
		return err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range []string{Pins, Env, Overlay, Secrets, Meta} {
			if _, err := tx.CreateBucketIfNotExists([]byte(b)); err != nil {
				return err
			}
		}
		mb := tx.Bucket([]byte(Meta))
		if err := mb.Put([]byte("schema"), []byte(strconv.Itoa(schemaVersion))); err != nil {
			return err
		}
		return mb.Put([]byte("vault_id"), []byte(newVaultID()))
	})
	closeErr := db.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// noSync skips fsync on commit; only tests turn it on via DisableSyncForTests.
var noSync atomic.Bool

// DisableSyncForTests makes commits skip fsync, which is slow on darwin.
func DisableSyncForTests() { noSync.Store(true) }

func Update(fn func(*Tx) error) error {
	path := Path()
	if err := create(path); err != nil {
		return err
	}
	tightenMode(path)
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: writeLockTimeout, NoSync: noSync.Load()})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Update(func(tx *bbolt.Tx) error {
		t := &Tx{tx}
		if err := checkSchema(t); err != nil {
			return err
		}
		// A vault with no vault_id predates per-vault keyring accounts; backfill it once (R1).
		if _, ok := t.Get(Meta, "vault_id"); !ok {
			if err := t.Put(Meta, "vault_id", newVaultID()); err != nil {
				return err
			}
		}
		return fn(t)
	})
}

// newVaultID is 16 random hex bytes, the identity its master key is stored under in the keyring (vault-key/<id>).
func newVaultID() string {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return hex.EncodeToString(id)
}

// vaultID reads the tx's identity, never empty on a writable Tx (Update backfills it, create writes it).
func vaultID(t *Tx) string {
	id, _ := t.Get(Meta, "vault_id")
	return id
}

type Tx struct{ tx *bbolt.Tx }

func (t *Tx) Get(bucket, key string) (string, bool) {
	if t.tx == nil {
		return "", false
	}
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return "", false
	}
	v := b.Get([]byte(key))
	return string(v), v != nil
}

func (t *Tx) All(bucket string) map[string]string {
	m := map[string]string{}
	if t.tx != nil {
		if b := t.tx.Bucket([]byte(bucket)); b != nil {
			_ = b.ForEach(func(k, v []byte) error {
				if v != nil {
					m[string(k)] = string(v)
				}
				return nil
			})
		}
	}
	return m
}

// Names lists bucket's keys in bbolt's byte order (already sorted), never copying values.
func (t *Tx) Names(bucket string) []string {
	names := make([]string, 0)
	if t.tx == nil {
		return names
	}
	if b := t.tx.Bucket([]byte(bucket)); b != nil {
		_ = b.ForEach(func(k, v []byte) error {
			if v != nil {
				names = append(names, string(k))
			}
			return nil
		})
	}
	return names
}

func (t *Tx) Put(bucket, key, val string) error {
	if t.tx == nil || !t.tx.Writable() {
		return bolterrors.ErrTxNotWritable
	}
	b := t.tx.Bucket([]byte(bucket))
	if val == "" {
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	}
	if b == nil {
		var err error
		b, err = t.tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
	}
	return b.Put([]byte(key), []byte(val))
}

type Record struct {
	Sealed       []byte    `json:"ct"`
	Kid          string    `json:"kid"`
	Prefix       string    `json:"prefix,omitempty"`
	Len          int       `json:"len,omitempty"`
	Label        string    `json:"label,omitempty"` // plaintext display label, never secret
	SetAt        time.Time `json:"set_at,omitzero"`
	VerifiedAt   time.Time `json:"verified_at,omitzero"`
	VerifyStatus string    `json:"verify_status,omitempty"`
}

// Record decodes name's stored record; found is false only for an absent key, a decode failure is an error.
func (t *Tx) Record(name string) (Record, bool, error) {
	return t.RecordIn(Secrets, name)
}

// RecordIn reads an encrypted record from its collection without exposing it as a credential.
func (t *Tx) RecordIn(bucket, name string) (Record, bool, error) {
	v, ok := t.Get(bucket, name)
	if !ok {
		return Record{}, false, nil
	}
	var r Record
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return Record{}, false, fmt.Errorf("vault: record %q is corrupt: %w", name, err)
	}
	return r, true, nil
}

// Records decodes every stored record, reporting the first decode error rather than dropping the entry (R5).
func (t *Tx) Records() (map[string]Record, error) {
	m := map[string]Record{}
	for _, n := range t.Names(Secrets) {
		r, ok, err := t.Record(n)
		if err != nil {
			return nil, err
		}
		if ok {
			m[n] = r
		}
	}
	return m, nil
}

func (t *Tx) PutRecord(name string, r Record) error {
	return t.PutRecordIn(Secrets, name, r)
}

// PutRecordIn stores a record in its collection; a zero record removes it.
func (t *Tx) PutRecordIn(bucket, name string, r Record) error {
	if len(r.Sealed) == 0 && r.Kid == "" && r.Prefix == "" && r.Len == 0 && r.Label == "" && r.SetAt.IsZero() && r.VerifiedAt.IsZero() && r.VerifyStatus == "" {
		return t.Put(bucket, name, "")
	}
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return t.Put(bucket, name, string(v))
}

// Retired returns the retired key IDs recorded by committed rotations, or an error for corrupt metadata.
func Retired(t *Tx) ([]string, error) {
	raw, ok := t.Get(Meta, "retired")
	if !ok {
		return nil, nil
	}
	var kids []string
	if err := json.Unmarshal([]byte(raw), &kids); err != nil {
		return nil, fmt.Errorf("vault: retired key IDs are corrupt: %w", err)
	}
	return kids, nil
}

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

// parseSchema reads a stamped layout number; unset or malformed is 0.
func parseSchema(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// openReadOnly opens the bbolt file at path read-only, bounded by readLockTimeout.
func openReadOnly(path string) (*bbolt.DB, error) {
	tightenMode(path)
	return bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: readLockTimeout})
}

// looseMode reports a vault file open to anyone but its owner (never on Windows).
func looseMode(fi os.FileInfo) bool { return runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 }

// tightenMode chmods an existing vault looser than 0600 to 0600, best-effort; the warning names the path only.
func tightenMode(path string) {
	fi, err := os.Stat(path)
	if err != nil || !looseMode(fi) {
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "vault: %s is mode %04o and chmod 0600 failed: %v\n", path, fi.Mode().Perm(), err)
	}
}

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
