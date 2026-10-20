package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
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

// MetaFile is names-only credential metadata, never a secret value.
func MetaFile() string { return filepath.Join(paths.EnvDir(), "secrets.meta.json") }

// errImportIncomplete marks an import taken while the keyring did not answer; it is never persisted.
var errImportIncomplete = errors.New("keyring did not answer; metadata not written")

// LoadMeta returns every row keyed by name; a missing file runs the import, persisted only when complete.
func LoadMeta() (map[string]Meta, error) {
	if b, err := os.ReadFile(MetaFile()); err == nil { // lock-free: AtomicWrite's rename never exposes a partial file
		return parseMetaDoc(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var out map[string]Meta
	err := withStoreLock(func() error {
		creds, imported, err := readMetaLocked() // re-read: another process may have written it since
		out = creds
		switch {
		case errors.Is(err, errImportIncomplete):
			return nil // next run (keyring hopefully up) retries the import
		case err != nil || !imported:
			return err
		}
		return writeMeta(creds)
	})
	return out, err
}

// readMetaLocked reads the meta file under the store lock; a missing one is imported, partial = errImportIncomplete.
func readMetaLocked() (creds map[string]Meta, imported bool, err error) {
	b, err := os.ReadFile(MetaFile())
	if os.IsNotExist(err) {
		creds, complete := importCredentials()
		if !complete {
			return creds, true, errImportIncomplete
		}
		return creds, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	creds, err = parseMetaDoc(b)
	return creds, false, err
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

// writeMeta persists creds (the caller holds the store lock).
func writeMeta(creds map[string]Meta) error {
	b, err := json.MarshalIndent(metaDoc{Version: metaVersion, Credentials: creds}, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(MetaFile(), b, 0o600)
}

// candidateNames is every name worth checking during import: file plane, providers' auth.env, custom.env @keyring refs.
func candidateNames(file map[string]string) map[string]bool {
	names := map[string]bool{}
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
// discovered at read time, not recorded). complete is false if any lookup hit ErrNoAnswer or
// ErrUnavailable (an installed tool that just failed); keyring.ErrNoTool (no tool on PATH at all, or
// an unsupported OS) is a genuine file-only install and does not block completion (R4).
func importCredentials() (creds map[string]Meta, complete bool) {
	file := ReadAll()
	creds = map[string]Meta{}
	complete = true
	for n := range candidateNames(file) {
		v, found, err := keyringLookup(n)
		switch {
		case errors.Is(err, keyring.ErrNoTool):
			// no keyring tool ever existed here: nothing to retry later.
		case errors.Is(err, keyring.ErrNoAnswer), errors.Is(err, keyring.ErrUnavailable):
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
	return editStore(func(s *state) {
		s.meta[name] = Meta{Plane: plane, Prefix: metaPrefix(value), Len: len(value), SetAt: time.Now()}
	})
}

// removeMeta drops name's row; a name with no row is a no-op.
func removeMeta(name string) error {
	return editStore(func(s *state) { delete(s.meta, name) })
}

// RecordProbe sets VerifyStatus/VerifiedAt on name's existing row; a name with no row is a no-op.
func RecordProbe(name, status string, at time.Time) error {
	return editStore(func(s *state) {
		if m, ok := s.meta[name]; ok {
			m.VerifyStatus, m.VerifiedAt = status, at
			s.meta[name] = m
		}
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
