package memory

import (
	"context"
	"crypto/rand"
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

type boltVectorStore struct{}

var vectorStore = &boltVectorStore{}

// VectorWrite replaces a record or atomically merges a mint while releasing its claim.
type VectorWrite struct {
	Profile string
	Record  vectorRecord
	Owner   int64
	Started int64
	Merge   func(vectorRecord) vectorRecord
}

func (s *boltVectorStore) Put(write VectorWrite) (stored bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		record := write.Record
		if write.Merge != nil {
			if !claimOwnedBy(tx, write.Profile, write.Owner) { // taken over or expired: refuse, stored stays false
				return nil
			}
			if err := dropClaim(tx, write.Profile, write.Owner); err != nil {
				return err
			}
			current := decodeEmbedRecord(tx, write.Profile)
			if current.MintedAt >= write.Started && current.MintedAt <= time.Now().UnixNano() {
				return nil
			}
			record = write.Merge(current)
		}
		stored = true
		return putEmbedRecord(tx, write.Profile, record)
	})
	return stored, err
}

const (
	embedValidity   = time.Hour        // a minted record is left alone for this long
	embedOverrideAt = 8 * time.Minute  // --reembed opens in the record's final stretch of the lease
	embedClaimTTL   = 2 * time.Minute  // a minting claim older than this is abandoned
	embedClaimSlack = 30 * time.Second // added to the caller's ctx deadline when sizing a claim
)

// Cache profiles hold JSON metadata and binary vector rows; claims hold profile -> expiry+owner.
var (
	embedCacheBucket = []byte("cache")
	embedClaimBucket = []byte("claims")
	embedRowsBucket  = []byte("rows")
	embedMetadataKey = []byte("metadata")
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
	// ErrEmbedSuperseded means a newer record landed during the mint, so this run's vectors were discarded.
	ErrEmbedSuperseded = errors.New("a newer mint landed first; this run's vectors were discarded")
	// ErrReembedEarly means --reembed was asked before the override window opened.
	ErrReembedEarly = errors.New("re-embed is not open yet")
)

// embedDBPath is the one bbolt file holding every profile's entry-vector record, scoped like StateDir.
func embedDBPath() string { return filepath.Join(StateDir(), "embeddings.db") }

func SafeProfile(profile string) bool { return safeProfile.MatchString(profile) }

func MintedTime(f vectorRecord) time.Time { return time.Unix(0, f.MintedAt) }

// embedValid reports whether f was minted less than embedValidity before now; a future MintedAt is not valid.
func embedValid(f vectorRecord, now time.Time) bool {
	return f.MintedAt > 0 && !MintedTime(f).After(now) && now.Sub(MintedTime(f)) < embedValidity
}

// reembedOpensAt reports the override opening time and whether it is open now.
func reembedOpensAt(f vectorRecord, now time.Time) (time.Time, bool) {
	if !embedValid(f, now) {
		return time.Time{}, true
	}
	at := MintedTime(f).Add(embedValidity - embedOverrideAt)
	return at, !now.Before(at)
}

// ReembedWindow reads profile's record and reports when --reembed opens and whether it is open now.
func ReembedWindow(profile string, now time.Time) (opensAt time.Time, open bool, err error) {
	if !SafeProfile(profile) {
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
func readEmbedCache(profile string) (vectorRecord, error) {
	return vectorStore.Vectors(profile)
}

func (s *boltVectorStore) Vectors(profile string) (vectorRecord, error) {
	db, err := bbolt.Open(embedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: embedReadTimeout})
	if errors.Is(err, os.ErrNotExist) {
		return vectorRecord{}, nil
	}
	if err != nil {
		return vectorRecord{}, err
	}
	defer db.Close()
	var f vectorRecord
	err = db.View(func(tx *bbolt.Tx) error {
		f = decodeEmbedRecord(tx, profile)
		return nil
	})
	return f, err
}

func decodeEmbedMetadata(tx *bbolt.Tx, profile string) vectorRecord {
	b := tx.Bucket(embedCacheBucket)
	if b == nil || b.Bucket([]byte(profile)) == nil {
		return vectorRecord{}
	}
	var f vectorRecord
	if !schemas.DecodeOK(embedFormat, b.Bucket([]byte(profile)).Get(embedMetadataKey), &f) {
		return vectorRecord{}
	}
	return f
}

func decodeEmbedRecord(tx *bbolt.Tx, profile string) vectorRecord {
	f := decodeEmbedMetadata(tx, profile)
	if f.Rows == nil {
		return f
	}
	rows := tx.Bucket(embedCacheBucket).Bucket([]byte(profile)).Bucket(embedRowsBucket)
	if rows == nil {
		return vectorRecord{}
	}
	for id, row := range f.Rows {
		raw := rows.Get([]byte(id))
		if raw == nil || len(raw)%4 != 0 {
			delete(f.Rows, id)
			continue
		}
		row.Vec = string(raw)
		f.Rows[id] = row
	}
	return f
}

// updateEmbedDB runs fn in one short read-write transaction on embeddings.db, creating the file and buckets.
func updateEmbedDB(fn func(tx *bbolt.Tx) error) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	db, err := bbolt.Open(embedDBPath(), 0o600, &bbolt.Options{Timeout: embedWriteTimeout})
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

func deleteEmbedRecord(cache *bbolt.Bucket, profile string) error {
	key := []byte(profile)
	if cache.Bucket(key) != nil {
		return cache.DeleteBucket(key)
	}
	return cache.Delete(key)
}

func putEmbedRecord(tx *bbolt.Tx, profile string, f vectorRecord) error {
	f.Schema = embedCacheSchema
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	cache := tx.Bucket(embedCacheBucket)
	if err := deleteEmbedRecord(cache, profile); err != nil {
		return err
	}
	b, err := cache.CreateBucket([]byte(profile))
	if err != nil {
		return err
	}
	if err := b.Put(embedMetadataKey, raw); err != nil {
		return err
	}
	rows, err := b.CreateBucket(embedRowsBucket)
	if err != nil {
		return err
	}
	for id, row := range f.Rows {
		if err := rows.Put([]byte(id), []byte(row.Vec)); err != nil {
			return err
		}
	}
	return nil
}

// newEmbedOwner is a random claim token, distinct from the mint's start time.
func newEmbedOwner() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b[:])), nil
}

// embedClaimExpiry is ctx's deadline plus slack when it has one, else embedClaimTTL, counted from now.
func embedClaimExpiry(ctx context.Context, now time.Time) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl.Add(embedClaimSlack)
	}
	return now.Add(embedClaimTTL)
}

// Claim takes profile's claim for owner until expiry; held is the live claim's expiry when another has it.
func (s *boltVectorStore) Claim(profile string, owner int64, now, expiry time.Time) (held time.Time, ok bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		b := tx.Bucket(embedClaimBucket)
		if v := b.Get([]byte(profile)); len(v) == 16 {
			if exp := time.Unix(0, int64(binary.BigEndian.Uint64(v))); exp.After(now) {
				held = exp
				return nil
			}
		}
		var v [16]byte
		binary.BigEndian.PutUint64(v[:8], uint64(expiry.UnixNano()))
		binary.BigEndian.PutUint64(v[8:], uint64(owner))
		ok = true
		return b.Put([]byte(profile), v[:])
	})
	return held, ok, err
}

// claimOwnedBy reports whether profile's claim record names owner.
func claimOwnedBy(tx *bbolt.Tx, profile string, owner int64) bool {
	v := tx.Bucket(embedClaimBucket).Get([]byte(profile))
	return len(v) == 16 && int64(binary.BigEndian.Uint64(v[8:])) == owner
}

func dropClaim(tx *bbolt.Tx, profile string, owner int64) error {
	if claimOwnedBy(tx, profile, owner) {
		return tx.Bucket(embedClaimBucket).Delete([]byte(profile))
	}
	return nil
}

// Release drops owner's claim on profile and returns the failure that left it behind; a taken-over claim is left alone.
func (s *boltVectorStore) Release(profile string, owner int64) error {
	return updateEmbedDB(func(tx *bbolt.Tx) error { return dropClaim(tx, profile, owner) })
}

// RefreshDue reads the mint lease and claim without taking a write lock or opening an endpoint.
func (s *boltVectorStore) RefreshDue(profile string, now time.Time) bool {
	if !SafeProfile(profile) {
		return false
	}
	db, err := bbolt.Open(embedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Nanosecond})
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	defer db.Close()
	due := false
	err = db.View(func(tx *bbolt.Tx) error {
		if embedValid(decodeEmbedMetadata(tx, profile), now) {
			return nil
		}
		if b := tx.Bucket(embedClaimBucket); b != nil {
			if v := b.Get([]byte(profile)); len(v) == 16 && time.Unix(0, int64(binary.BigEndian.Uint64(v))).After(now) {
				return nil
			}
		}
		due = true
		return nil
	})
	return err == nil && due
}
