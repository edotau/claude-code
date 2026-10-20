package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	if n > 1 {
		return fmt.Errorf("vault: written by a newer claude-code (schema %d)", n)
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
		return tx.Bucket([]byte(Meta)).Put([]byte("schema"), []byte("1"))
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
		return fn(t)
	})
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

func (t *Tx) Names(bucket string) []string {
	names := make([]string, 0)
	for k := range t.All(bucket) {
		names = append(names, k)
	}
	sort.Strings(names)
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

func (t *Tx) Record(name string) (Record, bool) {
	v, ok := t.Get(Secrets, name)
	var r Record
	if !ok || json.Unmarshal([]byte(v), &r) != nil {
		return Record{}, false
	}
	return r, true
}

func (t *Tx) Records() map[string]Record {
	m := map[string]Record{}
	for _, n := range t.Names(Secrets) {
		if r, ok := t.Record(n); ok {
			m[n] = r
		}
	}
	return m
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
