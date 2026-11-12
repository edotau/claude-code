package memory

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

type boltVectorStore struct{}

var vectorStore = &boltVectorStore{}

// vectorWrite atomically merges a mint while releasing its claim.
type vectorWrite struct {
	Profile string
	Owner   int64
	Started int64
	Merge   func(vectorRecord) vectorRecord
}

func (s *boltVectorStore) Put(write vectorWrite) (stored bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
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
		stored = true
		return putEmbedRecord(tx, write.Profile, write.Merge(current))
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
	// errEmbedValid means the profile's record is inside its validity lease, so nothing was minted.
	errEmbedValid = errors.New("embedding cache is still valid")
	// errEmbedBusy means another session holds the minting claim.
	errEmbedBusy = errors.New("another session is minting the embedding cache")
	// errEmbedSuperseded means a newer record landed during the mint, so this run's vectors were discarded.
	errEmbedSuperseded = errors.New("a newer mint landed first; this run's vectors were discarded")
	// errReembedEarly means --reembed was asked before the override window opened.
	errReembedEarly = errors.New("re-embed is not open yet")
)

// embedDBPath is the one bbolt file holding every profile's entry-vector record, scoped like StateDir.
func embedDBPath() string { return filepath.Join(StateDir(), "embeddings.db") }

func mintedTime(f vectorRecord) time.Time { return time.Unix(0, f.MintedAt) }

// embedValid reports whether f was minted less than embedValidity before now; a future MintedAt is not valid.
func embedValid(f vectorRecord, now time.Time) bool {
	return f.MintedAt > 0 && !mintedTime(f).After(now) && now.Sub(mintedTime(f)) < embedValidity
}

// reembedOpensAt reports the override opening time and whether it is open now.
func reembedOpensAt(f vectorRecord, now time.Time) (time.Time, bool) {
	if !embedValid(f, now) {
		return time.Time{}, true
	}
	at := mintedTime(f).Add(embedValidity - embedOverrideAt)
	return at, !now.Before(at)
}

// reembedWindow reads profile's record and reports when --reembed opens and whether it is open now.
func reembedWindow(profile string, now time.Time) (opensAt time.Time, open bool, err error) {
	if !safeProfile.MatchString(profile) {
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

// embedDBBroken reports an open failure that blames the file's bytes: not a lock timeout, a permission or a missing file.
func embedDBBroken(err error) bool {
	return err != nil && !errors.Is(err, bolterrors.ErrTimeout) && !errors.Is(err, fs.ErrPermission) && !errors.Is(err, os.ErrNotExist)
}

// quarantineEmbedDB moves a damaged or zero-byte embeddings.db aside so the next write open creates a fresh one.
func quarantineEmbedDB() error { return os.Rename(embedDBPath(), embedDBPath()+".damaged") }

// RefreshDue reads the mint lease and claim without taking a write lock or opening an endpoint; host is the
// profile's current endpoint host, so a record minted against another host is due even inside its lease.
func (s *boltVectorStore) RefreshDue(profile, host string, now time.Time) bool {
	if !safeProfile.MatchString(profile) {
		return false
	}
	db, err := bbolt.Open(embedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Nanosecond})
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return embedDBBroken(err) // the spawned mint quarantines it; a busy or unreadable-by-permission file waits
	}
	defer db.Close()
	due := false
	err = db.View(func(tx *bbolt.Tx) error {
		rec := decodeEmbedMetadata(tx, profile)
		if embedValid(rec, now) && rec.Namespace == embedNamespace(profile, host, rec.Model) {
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

// embedProvider is the first of the session and claude-record profiles that can embed, matching NewSemantic's order.
func embedProvider() *providers.Provider {
	reg, err := providers.Load()
	if err != nil {
		return nil
	}
	for name := range embedProfiles {
		if p := reg.Providers[name]; p != nil && p.Preset == providers.PresetDatabricks {
			return p
		}
	}
	return nil
}

// embedProfile names embedProvider's profile, "" when none can embed.
func embedProfile() string {
	if p := embedProvider(); p != nil {
		return p.Name
	}
	return ""
}

// refreshEmbeddings starts a detached mint at most once per claim TTL; failed launches release the slot.
func refreshEmbeddings(root, profile, host string, now time.Time) bool {
	stamp := filepath.Join(StateDir(), embedCachePre+profile+".refresh")
	stampFresh := func() bool {
		st, err := os.Stat(stamp)
		return err == nil && now.Sub(st.ModTime()) < embedClaimTTL
	}
	if IsUpdateChild() || !safeProfile.MatchString(profile) || embedBackedOff(profile, now) || stampFresh() {
		return false
	}
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return false
	}
	spawned := false
	_, _ = paths.TryFileLock(stamp+".lock", 0, func() error {
		if stampFresh() {
			return nil
		}
		if !vectorStore.RefreshDue(profile, host, now) {
			return nil
		}
		if err := paths.AtomicWrite(stamp, nil, 0o600); err != nil {
			return err
		}
		spawned = spawnEmbedRefresh(root, profile)
		if !spawned {
			_ = os.Remove(stamp)
		}
		return nil
	})
	return spawned
}

// embedBackedOff reports a recent endpoint failure, when a mint would only fail and retry.
func embedBackedOff(profile string, now time.Time) bool {
	st, err := os.Stat(filepath.Join(StateDir(), embedCachePre+profile+".backoff"))
	return err == nil && now.Sub(st.ModTime()) < embedBackoff
}

// spawnEmbedRefresh uses the mint-only index path so automatic refresh never settles the bank.
func spawnEmbedRefresh(root, profile string) bool {
	args := []string{"memory", "index", "--mint-only", "--repo", root}
	return spawnSelf(args, root, "", providers.EnvSessionProfile+"="+profile, UpdateChildEnv+"=1")
}

// RefreshStaleEmbeddings schedules only stale vector mints; no endpoint work runs in the hook.
func RefreshStaleEmbeddings(cwd string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if IsUpdateChild() || !bankExists(cwd) {
		return
	}
	if p := embedProvider(); p != nil {
		refreshEmbeddings(cwd, p.Name, p.Host, time.Now())
	}
}

func hitHash(h SearchHit) string { return textutil.SHA256Hex([]byte(h.File + "\x00" + h.Block)) }

func newSemantic(query []float32, rows map[string]vectorRow) *Semantic {
	s := &Semantic{query: query, rows: rows, blocks: make(map[string]string, len(rows))}
	for id, row := range rows {
		if row.BlockHash != "" {
			s.blocks[row.BlockHash] = id
		}
	}
	return s
}

// CosineHit reuses the minted block hash and ID without constructing an Entry.
func (s *Semantic) CosineHit(h SearchHit) (float64, bool) {
	id, ok := s.blocks[hitHash(h)]
	if !ok || h.ID != "" && h.ID != id {
		return 0, false
	}
	return s.cosineRow(s.rows[id])
}

func (s *Semantic) cosineRow(row vectorRow) (float64, bool) {
	if len(row.Vec) != 4*len(s.query) {
		return 0, false
	}
	var dot float64
	for i, q := range s.query {
		v := math.Float32frombits(binary.LittleEndian.Uint32([]byte(row.Vec[4*i : 4*i+4])))
		dot += float64(v) * float64(q)
	}
	return dot, true
}

// Candidates scores the already loaded corpus segments without parsing bank files or constructing entries.
func (s *Semantic) Candidates(files []corpusFile, segs []segView, k int, floor float64, skip func(SearchHit) bool) (hits []SearchHit, skipped bool) {
	var all []scoredHit
	for i, seg := range segs {
		if seg == nil || files[i].Bank == "archive" {
			continue
		}
		for doc := range seg.Docs() {
			block, _ := seg.Doc(doc)
			h, cos, ok := s.hitFor(files[i], block, floor)
			if !ok {
				continue
			}
			if skip != nil && skip(h) {
				skipped = true
				continue
			}
			all = append(all, scoredHit{h, cos})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].cos > all[j].cos })
	for _, a := range all[:min(k, len(all))] {
		hits = append(hits, a.hit)
	}
	return hits, skipped
}

type scoredHit struct {
	hit SearchHit
	cos float64
}

// hitFor builds the hit for block of file f and its cosine; false for a struck-out block or one with no embedding at floor.
func (s *Semantic) hitFor(f corpusFile, block string, floor float64) (SearchHit, float64, bool) {
	if isSupersededTitle(textutil.FirstLine(block)) {
		return SearchHit{}, 0, false
	}
	h := SearchHit{Bank: f.Bank, File: filepath.Base(f.Path), Block: block}
	h.ID = s.blocks[hitHash(h)]
	cos, ok := s.cosineRow(s.rows[h.ID])
	if h.ID == "" || !ok || cos < floor {
		return h, 0, false
	}
	h.Dir = ownerDir(f.Bank, f.Path)
	return h, cos, true
}
