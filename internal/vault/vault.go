package vault

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"go.etcd.io/bbolt"
)

const (
	Pins    = "pins"
	Env     = "env"
	Overlay = "overlay"
	Secrets = "secrets"
	Meta    = "meta"
)

var (
	ErrLocked   = errors.New("vault: no master key (keyring, HARNESS_VAULT_KEY or key_command)")
	ErrWrongKey = errors.New("vault: master key does not match this vault")
)

// schemaVersion is the current on-disk layout; bumped for the HKDF subkey change (Phase 4b). A
// vault at an older schema with secret records is refused — this build cannot re-seal them; one
// with zero secrets is upgraded in place by the next Update.
const schemaVersion = 2

// View and Update are transaction BOUNDARIES: never call View, Update, UpdateSealed, or MasterKey
// (it opens its own View) from inside another's callback. bbolt is not reentrant across process
// file locks, so a same-goroutine nested open blocks on the outer transaction's own flock until
// ITS Timeout elapses and then fails with bbolt.ErrTimeout — it does not hang forever, but it is
// never correct. Package helpers that must run inside an already-open transaction (e.g. a future
// tx-scoped key/record read reused by UpdateSealed's callback) take the open *Tx as a parameter
// instead of reopening the database; this is an architectural contract Go cannot enforce at
// compile time, not a runtime-detected guarantee (no process-wide or goroutine-keyed guard is
// maintained here — see the retired runtime.Stack-based one in git history).
func Path() string                  { return filepath.Join(paths.ConfigDir(), "state", "vault.db") }
func View(fn func(*Tx) error) error { return ViewAt(Path(), fn) }

func ViewAt(path string, fn func(*Tx) error) error {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
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
	if n < schemaVersion && len(t.Names(Secrets)) > 0 {
		return fmt.Errorf("vault schema %d: written by an older claude-code (pre-HKDF format); "+
			"this build cannot read its secrets. Move state/vault.db aside and re-enter secrets with "+
			"claude-code secrets set, or run the older binary to export them first", n)
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
	defer os.Remove(tmp)
	if err = f.Close(); err != nil {
		return err
	}
	db, err := bbolt.Open(tmp, 0600, nil)
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
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(tmp, path); errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

func Update(fn func(*Tx) error) error {
	path := Path()
	if err := create(path); err != nil {
		return err
	}
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: 5 * time.Second})
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
		// checkSchema already refused an older schema with secrets; a secret-free older schema
		// upgrades in place here, with nothing to re-seal (Phase 4b).
		if s, _ := t.Get(Meta, "schema"); s != strconv.Itoa(schemaVersion) {
			if err := t.Put(Meta, "schema", strconv.Itoa(schemaVersion)); err != nil {
				return err
			}
		}
		return fn(t)
	})
}

// newVaultID is 16 random bytes hex-encoded, the per-vault identity under which its master key
// is stored in the keyring (vault-key/<id>); it lets a second config root bootstrap its own key.
func newVaultID() string {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return hex.EncodeToString(id)
}

// vaultID reads the current tx's identity; callers only reach it after Update's backfill or
// create()'s write, so it is never empty on a writable Tx.
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

// Names lists bucket's keys in bbolt's byte order (already sorted), never copying values; checkSchema runs it per tx.
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
		return bbolt.ErrTxNotWritable
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
	SetAt        time.Time `json:"set_at,omitzero"`
	VerifiedAt   time.Time `json:"verified_at,omitzero"`
	VerifyStatus string    `json:"verify_status,omitempty"`
}

// Record decodes name's stored record. found is false only for a genuinely absent key; a stored
// value that fails to decode is reported as an error, never silently collapsed to not-found (R5
// — Phase 3's credential fallback and export must see corruption, not a false miss).
func (t *Tx) Record(name string) (Record, bool, error) {
	v, ok := t.Get(Secrets, name)
	if !ok {
		return Record{}, false, nil
	}
	var r Record
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return Record{}, false, fmt.Errorf("vault: record %q is corrupt: %w", name, err)
	}
	return r, true, nil
}

// Records decodes every stored record; it stops and reports the first decode error rather than
// silently omitting a corrupt entry from the map (R5).
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
	if len(r.Sealed) == 0 && r.Kid == "" && r.Prefix == "" && r.Len == 0 && r.SetAt.IsZero() && r.VerifiedAt.IsZero() && r.VerifyStatus == "" {
		return t.Put(Secrets, name, "")
	}
	v, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return t.Put(Secrets, name, string(v))
}

// Retired returns the retired key IDs recorded by committed rotations.
func Retired(t *Tx) []string {
	raw, _ := t.Get(Meta, "retired")
	var kids []string
	if json.Unmarshal([]byte(raw), &kids) != nil {
		return nil
	}
	return kids
}
