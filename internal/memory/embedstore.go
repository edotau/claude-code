package memory

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"

	"github.com/edotau/claude-code/internal/schemas"
)

const (
	embedValidity   = time.Hour       // a minted record is left alone for this long
	embedOverrideAt = 8 * time.Minute // --reembed opens in the record's final stretch of the lease
	embedClaimTTL   = 2 * time.Minute // a minting claim older than this is abandoned
)

// Buckets of embeddings.db: cache holds profile -> embedCacheFile JSON, claims holds profile -> expiry+owner.
var (
	embedCacheBucket = []byte("cache")
	embedClaimBucket = []byte("claims")
)

// Timeouts for the bbolt file lock; a var so tests can shorten them.
var (
	embedReadTimeout  = 300 * time.Millisecond
	embedWriteTimeout = 5 * time.Second
)

var (
	// ErrEmbedValid means the profile's record is inside its validity lease, so nothing was minted.
	ErrEmbedValid = errors.New("embedding cache is still valid")
	// ErrEmbedBusy means another session holds the minting claim.
	ErrEmbedBusy = errors.New("another session is minting the embedding cache")
	// ErrReembedEarly means --reembed was asked before the override window opened.
	ErrReembedEarly = errors.New("re-embed is not open yet")
)

// EmbedDBPath is the one bbolt file holding every profile's entry-vector record, scoped like StateDir.
func EmbedDBPath() string { return filepath.Join(StateDir(), "embeddings.db") }

func safeEmbedProfile(profile string) bool { return safeProfile.MatchString(profile) }

func mintedTime(f embedCacheFile) time.Time { return time.Unix(0, f.MintedAt) }

// embedValid reports whether f was minted less than embedValidity before now.
func embedValid(f embedCacheFile, now time.Time) bool {
	return f.MintedAt > 0 && now.Sub(mintedTime(f)) < embedValidity
}

// reembedOpensAt is when --reembed becomes allowed for f (the zero time if it already is) and whether it is allowed now.
func reembedOpensAt(f embedCacheFile, now time.Time) (time.Time, bool) {
	if !embedValid(f, now) {
		return time.Time{}, true
	}
	at := mintedTime(f).Add(embedValidity - embedOverrideAt)
	return at, !now.Before(at)
}

// ReembedWindow reads profile's record and reports when --reembed opens and whether it is open now.
func ReembedWindow(profile string, now time.Time) (opensAt time.Time, open bool, err error) {
	if !safeEmbedProfile(profile) {
		return time.Time{}, false, fmt.Errorf("embeddings: unsafe profile name %q", profile)
	}
	f, err := readEmbedCache(profile)
	if err != nil {
		return time.Time{}, false, err
	}
	at, open := reembedOpensAt(f, now)
	return at, open, nil
}

// readEmbedCache reads profile's record without blocking long; a missing file or record is empty.
func readEmbedCache(profile string) (embedCacheFile, error) {
	db, err := bbolt.Open(EmbedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: embedReadTimeout})
	if errors.Is(err, os.ErrNotExist) {
		return embedCacheFile{}, nil
	}
	if err != nil {
		return embedCacheFile{}, err
	}
	defer db.Close()
	var f embedCacheFile
	err = db.View(func(tx *bbolt.Tx) error {
		f = decodeEmbedRecord(tx, profile)
		return nil
	})
	return f, err
}

// loadEmbedCache is readEmbedCache with every failure folded into an empty record.
func loadEmbedCache(profile string) embedCacheFile {
	if !safeEmbedProfile(profile) {
		return embedCacheFile{}
	}
	f, err := readEmbedCache(profile)
	if err != nil {
		return embedCacheFile{}
	}
	return f
}

func decodeEmbedRecord(tx *bbolt.Tx, profile string) embedCacheFile {
	b := tx.Bucket(embedCacheBucket)
	if b == nil {
		return embedCacheFile{}
	}
	raw := b.Get([]byte(profile))
	var f embedCacheFile
	if raw == nil || !schemas.DecodeOK(embedFormat, raw, &f) {
		return embedCacheFile{}
	}
	return f
}

// updateEmbedDB runs fn in one short read-write transaction on embeddings.db, creating the file and buckets.
func updateEmbedDB(fn func(tx *bbolt.Tx) error) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	db, err := bbolt.Open(EmbedDBPath(), 0o600, &bbolt.Options{Timeout: embedWriteTimeout})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{embedCacheBucket, embedClaimBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

func putEmbedRecord(tx *bbolt.Tx, profile string, f embedCacheFile) error {
	f.Schema = embedCacheSchema
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return tx.Bucket(embedCacheBucket).Put([]byte(profile), raw)
}

// saveEmbedCache stores f as profile's record unconditionally (one Put); the mint path uses commitEmbedMint.
func saveEmbedCache(profile string, f embedCacheFile) error {
	if !safeEmbedProfile(profile) {
		return fmt.Errorf("embeddings: unsafe profile name %q", profile)
	}
	return updateEmbedDB(func(tx *bbolt.Tx) error { return putEmbedRecord(tx, profile, f) })
}

// claimEmbedMint takes profile's minting claim for owner; held is the live claim's expiry when another owner has it.
func claimEmbedMint(profile string, owner int64, now time.Time) (held time.Time, ok bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		b := tx.Bucket(embedClaimBucket)
		if v := b.Get([]byte(profile)); len(v) == 16 {
			if exp := time.Unix(0, int64(binary.BigEndian.Uint64(v))); exp.After(now) {
				held = exp
				return nil
			}
		}
		var v [16]byte
		binary.BigEndian.PutUint64(v[:8], uint64(now.Add(embedClaimTTL).UnixNano()))
		binary.BigEndian.PutUint64(v[8:], uint64(owner))
		ok = true
		return b.Put([]byte(profile), v[:])
	})
	return held, ok, err
}

func dropClaim(tx *bbolt.Tx, profile string, owner int64) error {
	b := tx.Bucket(embedClaimBucket)
	if v := b.Get([]byte(profile)); len(v) == 16 && int64(binary.BigEndian.Uint64(v[8:])) == owner {
		return b.Delete([]byte(profile))
	}
	return nil
}

// releaseEmbedMint drops owner's claim on profile; a claim another owner took over is left alone.
func releaseEmbedMint(profile string, owner int64) {
	_ = updateEmbedDB(func(tx *bbolt.Tx) error { return dropClaim(tx, profile, owner) })
}

// commitEmbedMint stores build(current) as profile's record in one transaction and drops owner's claim. It discards
// the result (false) when a record was minted at or after started, so a mint that began earlier never overwrites it.
func commitEmbedMint(profile string, started int64, build func(cur embedCacheFile) embedCacheFile) (stored bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		if err := dropClaim(tx, profile, started); err != nil {
			return err
		}
		cur := decodeEmbedRecord(tx, profile)
		if cur.MintedAt >= started {
			return nil
		}
		stored = true
		return putEmbedRecord(tx, profile, build(cur))
	})
	return stored, err
}
