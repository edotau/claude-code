package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// Meta is one credential's names-only metadata: never a value, just where it lives and how long it is.
// Plane is "keyring"|"file". VerifyStatus is ""|"verified"|"rejected"|"unreachable". The name itself
// is the map key in metaDoc.Credentials, not stored here.
type Meta struct {
	Plane        string    `json:"plane"`
	Prefix       string    `json:"prefix,omitempty"`
	Len          int       `json:"len,omitempty"`
	SetAt        time.Time `json:"set_at,omitzero"`
	VerifiedAt   time.Time `json:"verified_at,omitzero"`
	VerifyStatus string    `json:"verify_status,omitempty"`
}

// metaVersion is the on-disk schema version this binary writes and the highest it will read.
const metaVersion = 1

// metaDoc is secrets.meta.json's shape; map keys sort automatically through encoding/json.
type metaDoc struct {
	Version     int             `json:"version"`
	Credentials map[string]Meta `json:"credentials"`
}

// MetaFile replaces indexFile: names-only credential metadata, never a secret value.
func MetaFile() string { return filepath.Join(paths.EnvDir(), "secrets.meta.json") }

// LoadMeta returns every credential's metadata, keyed by name. A missing file triggers a one-time
// import from secrets.index, secrets.env, the provider registry's auth.env, and custom.env's
// @keyring refs; the import is written once and never overwrites a file that already exists.
// An existing file is read without the lock: writes go through AtomicWrite's rename, so a reader
// sees the old or the new content, never a partial one.
func LoadMeta() (map[string]Meta, error) {
	if b, err := os.ReadFile(MetaFile()); err == nil {
		return parseMetaDoc(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var out map[string]Meta
	err := paths.WithFileLock(MetaFile(), func() error {
		// re-check inside the lock: another process may have written the file since the read above.
		b, err := os.ReadFile(MetaFile())
		if err == nil {
			out, err = parseMetaDoc(b)
			return err
		}
		if !os.IsNotExist(err) {
			return err
		}
		creds, complete := importCredentials()
		// An import taken while the keyring was down is missing its keyring-only names; don't
		// persist it, so the next run (keyring hopefully up) retries the import from scratch.
		if complete {
			if err := writeMetaLocked(creds); err != nil {
				return err
			}
		}
		out = creds
		return nil
	})
	return out, err
}

// readMetaLocked reads the meta file (the caller already holds its lock); a missing file runs the
// import. complete is false only when the import ran and the keyring did not answer.
func readMetaLocked() (creds map[string]Meta, imported, complete bool, err error) {
	b, err := os.ReadFile(MetaFile())
	if os.IsNotExist(err) {
		var done bool
		creds, done = importCredentials()
		return creds, true, done, nil
	}
	if err != nil {
		return nil, false, true, err
	}
	creds, err = parseMetaDoc(b)
	return creds, false, true, err
}

// parseMetaDoc unmarshals the meta file's bytes, rejecting a schema version newer than this binary writes.
func parseMetaDoc(b []byte) (map[string]Meta, error) {
	var doc metaDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", MetaFile(), err)
	}
	if doc.Version > metaVersion {
		return nil, fmt.Errorf("%s: written by a newer claude-code (version %d)", MetaFile(), doc.Version)
	}
	if doc.Credentials == nil {
		doc.Credentials = map[string]Meta{}
	}
	return doc.Credentials, nil
}

// writeMetaLocked persists creds (the caller already holds the lock).
func writeMetaLocked(creds map[string]Meta) error {
	b, err := json.MarshalIndent(metaDoc{Version: metaVersion, Credentials: creds}, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(MetaFile(), b, 0o600)
}

// editMeta loads the meta file under its lock, lets mutate change creds in place, and writes back
// only when mutate reports a change. An import taken while the keyring was down is rejected outright,
// so the next call (keyring hopefully up) retries the import instead of persisting a partial one.
func editMeta(mutate func(creds map[string]Meta) bool) error {
	return paths.WithFileLock(MetaFile(), func() error {
		creds, imported, complete, err := readMetaLocked()
		if err != nil {
			return err
		}
		if imported && !complete {
			return fmt.Errorf("keyring did not answer; metadata not written")
		}
		if !mutate(creds) {
			return nil
		}
		return writeMetaLocked(creds)
	})
}

// candidateNames is every name worth checking during import: the retired index, the file plane
// (reusing an already-loaded read), every provider's auth.env, and custom.env's @keyring refs.
func candidateNames(file map[string]string) map[string]bool {
	names := map[string]bool{}
	for _, n := range readIndex() {
		names[n] = true
	}
	for n := range file {
		names[n] = true
	}
	if reg, err := providers.Load(); err == nil {
		for _, p := range reg.Providers {
			if p.Auth.Env != "" {
				names[p.Auth.Env] = true
			}
		}
	}
	for n, v := range providers.CustomEnv() {
		if v == providers.KeyringRef {
			names[n] = true
		}
	}
	return names
}

// importCredentials discovers every name the harness knows of across every plane, keyringLookup only
// here; a name found in neither the keyring nor the file plane is dropped (env-only values are
// discovered at read time, not recorded). complete is false if any lookup hit ErrNoAnswer.
func importCredentials() (creds map[string]Meta, complete bool) {
	file := ReadAll()
	creds = map[string]Meta{}
	complete = true
	for n := range candidateNames(file) {
		v, found, err := keyringLookup(n)
		if err != nil {
			complete = false
		}
		if found {
			creds[n] = Meta{Plane: "keyring", Prefix: metaPrefix(v), Len: len(v)}
			continue
		}
		if v, ok := file[n]; ok && v != "" {
			creds[n] = Meta{Plane: "file", Prefix: metaPrefix(v), Len: len(v)}
		}
	}
	return creds, complete
}

// upsertMeta records name landing on plane ("keyring"|"file") with value's prefix/length, now; a
// fresh value is unverified, so any prior VerifyStatus/VerifiedAt is dropped.
func upsertMeta(name, plane, value string) error {
	return editMeta(func(creds map[string]Meta) bool {
		creds[name] = Meta{Plane: plane, Prefix: metaPrefix(value), Len: len(value), SetAt: time.Now()}
		return true
	})
}

// removeMeta drops name's row; a name with no row is a no-op.
func removeMeta(name string) error {
	return editMeta(func(creds map[string]Meta) bool {
		if _, ok := creds[name]; !ok {
			return false
		}
		delete(creds, name)
		return true
	})
}

// RecordProbe sets VerifyStatus/VerifiedAt on name's existing row; a name with no row is a no-op.
func RecordProbe(name, status string, at time.Time) error {
	return editMeta(func(creds map[string]Meta) bool {
		m, ok := creds[name]
		if !ok {
			return false
		}
		m.VerifyStatus, m.VerifiedAt = status, at
		creds[name] = m
		return true
	})
}

// CachedProbe reports m's cached verify result if it is still within ttl: ok is true only for
// verified/rejected; age is always now−VerifiedAt whenever VerifiedAt is set, cached or not.
func CachedProbe(m Meta, ttl time.Duration, now time.Time) (status string, age time.Duration, ok bool) {
	if m.VerifiedAt.IsZero() {
		return "", 0, false
	}
	age = now.Sub(m.VerifiedAt)
	if (m.VerifyStatus == "verified" || m.VerifyStatus == "rejected") && age < ttl {
		return m.VerifyStatus, age, true
	}
	return "", age, false
}

// metaPrefix is the vendor prefix safe to persist: dropped when it covers over a third of v, so a value made only
// of short dash-separated segments never lands in the meta file whole.
func metaPrefix(v string) string {
	if p := keyPrefix(v); len(p) <= len(v)/3 {
		return p
	}
	return ""
}
