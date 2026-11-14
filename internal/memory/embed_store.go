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
	"github.com/edotau/claude-code/internal/proc"
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
	Owner   int64
	Started int64
	Merge   func(vectorRecord) vectorRecord
}

func (s *boltVectorStore) Put(write vectorWrite) (stored bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		if !claimOwnedBy(tx, write.Owner) { // taken over or expired: refuse, stored stays false
			return nil
		}
		if err := dropClaim(tx, write.Owner); err != nil {
			return err
		}
		current := decodeEmbedRecord(tx)
		if current.MintedAt >= write.Started && current.MintedAt <= time.Now().UnixNano() {
			return nil
		}
		stored = true
		return putEmbedRecord(tx, write.Merge(current))
	})
	return stored, err
}

const (
	embedValidity   = time.Hour        // a minted record is left alone for this long
	embedOverrideAt = 8 * time.Minute  // --reembed opens in the record's final stretch of the lease
	embedClaimTTL   = 2 * time.Minute  // a minting claim older than this is abandoned
	embedClaimSlack = 30 * time.Second // added to the caller's ctx deadline when sizing a claim
)

// The shared cache holds JSON metadata and binary vector rows; claims hold one mint key -> expiry+owner.
var (
	embedVectorBucket = []byte("vectors")
	embedLegacyBucket = []byte("cache") // v2: one bucket per profile, folded into the shared one by FoldLegacy
	embedClaimBucket  = []byte("claims")
	embedClaimKey     = []byte("mint")
	embedRowsBucket   = []byte("rows")
	embedMetadataKey  = []byte("metadata")
)

// Timeouts for the bbolt file lock; a var so tests can shorten them.
var (
	embedReadTimeout  = 300 * time.Millisecond
	embedWriteTimeout = 5 * time.Second
)

var (
	// errEmbedValid means the cache record is inside its validity lease, so nothing was minted.
	errEmbedValid = errors.New("embedding cache is still valid")
	// errEmbedBusy means another session holds the minting claim.
	errEmbedBusy = errors.New("another session is minting the embedding cache")
	// errEmbedSuperseded means a newer record landed during the mint, so this run's vectors were discarded.
	errEmbedSuperseded = errors.New("a newer mint landed first; this run's vectors were discarded")
	// errReembedEarly means --reembed was asked before the override window opened.
	errReembedEarly = errors.New("re-embed is not open yet")
)

// openEmbedDB opens embeddings.db, refusing a file shorter than its page high-water mark (bbolt would SIGSEGV on read).
func openEmbedDB(opts *bbolt.Options) (*bbolt.DB, error) {
	db, err := bbolt.Open(embedDBPath(), 0o600, opts)
	if err != nil {
		return nil, err
	}
	var want int64
	_ = db.View(func(tx *bbolt.Tx) error { want = tx.Size(); return nil })
	if st, err := os.Stat(embedDBPath()); err != nil || st.Size() < want {
		_ = db.Close()
		return nil, errors.New("embeddings.db is truncated")
	}
	return db, nil
}

// embedDBPath is the one bbolt file holding the shared entry-vector record, scoped like StateDir.
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

// reembedWindow reads the shared record and reports when --reembed opens and whether it is open now.
func reembedWindow(now time.Time) (opensAt time.Time, open bool, err error) {
	f, err := readEmbedCache()
	if err != nil {
		return time.Time{}, false, err
	}
	at, open := reembedOpensAt(f, now)
	return at, open, nil
}

// readEmbedCache reads the shared record without blocking long; a missing file or record is empty. A var for tests.
var readEmbedCache = vectorStore.Vectors

func (s *boltVectorStore) Vectors() (vectorRecord, error) {
	db, err := openEmbedDB(&bbolt.Options{ReadOnly: true, Timeout: embedReadTimeout})
	if errors.Is(err, os.ErrNotExist) {
		return vectorRecord{}, nil
	}
	if err != nil {
		return vectorRecord{}, err
	}
	defer db.Close()
	var f vectorRecord
	err = db.View(func(tx *bbolt.Tx) error {
		f = decodeEmbedRecord(tx)
		return nil
	})
	return f, err
}

func decodeEmbedRecord(tx *bbolt.Tx) vectorRecord {
	return decodeVectorBucket(tx.Bucket(embedVectorBucket), embedFormat)
}

// decodeVectorMeta reads b's metadata at format f's version; a missing or mismatched one is the empty record.
func decodeVectorMeta(b *bbolt.Bucket, f schemas.Format) vectorRecord {
	var rec vectorRecord
	if b == nil || !schemas.DecodeOK(f, b.Get(embedMetadataKey), &rec) {
		return vectorRecord{}
	}
	return rec
}

// decodeVectorBucket is decodeVectorMeta plus the binary rows; a row that is missing or not whole float32s is dropped.
func decodeVectorBucket(b *bbolt.Bucket, f schemas.Format) vectorRecord {
	rec := decodeVectorMeta(b, f)
	if rec.Rows == nil {
		return rec
	}
	rows := b.Bucket(embedRowsBucket)
	if rows == nil {
		return vectorRecord{}
	}
	for id, row := range rec.Rows {
		raw := rows.Get([]byte(id))
		if raw == nil || len(raw)%4 != 0 {
			delete(rec.Rows, id)
			continue
		}
		row.Vec = string(raw)
		rec.Rows[id] = row
	}
	return rec
}

// updateEmbedDB runs fn in one short read-write transaction on embeddings.db, creating the file and buckets.
func updateEmbedDB(fn func(tx *bbolt.Tx) error) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	db, err := openEmbedDB(&bbolt.Options{Timeout: embedWriteTimeout})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{embedVectorBucket, embedClaimBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

func putEmbedRecord(tx *bbolt.Tx, f vectorRecord) error {
	f.Schema = embedCacheSchema
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := tx.DeleteBucket(embedVectorBucket); err != nil {
		return err
	}
	b, err := tx.CreateBucket(embedVectorBucket)
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

// Claim takes the mint claim for owner until expiry; held is the live claim's expiry when another has it.
func (s *boltVectorStore) Claim(owner int64, now, expiry time.Time) (held time.Time, ok bool, err error) {
	err = updateEmbedDB(func(tx *bbolt.Tx) error {
		b := tx.Bucket(embedClaimBucket)
		if v := b.Get(embedClaimKey); len(v) == 16 {
			if exp := time.Unix(0, int64(binary.BigEndian.Uint64(v))); exp.After(now) {
				held = exp
				return nil
			}
		}
		var v [16]byte
		binary.BigEndian.PutUint64(v[:8], uint64(expiry.UnixNano()))
		binary.BigEndian.PutUint64(v[8:], uint64(owner))
		ok = true
		return b.Put(embedClaimKey, v[:])
	})
	return held, ok, err
}

// claimOwnedBy reports whether the mint claim names owner.
func claimOwnedBy(tx *bbolt.Tx, owner int64) bool {
	v := tx.Bucket(embedClaimBucket).Get(embedClaimKey)
	return len(v) == 16 && int64(binary.BigEndian.Uint64(v[8:])) == owner
}

func dropClaim(tx *bbolt.Tx, owner int64) error {
	if claimOwnedBy(tx, owner) {
		return tx.Bucket(embedClaimBucket).Delete(embedClaimKey)
	}
	return nil
}

// Release drops owner's claim and returns the failure that left it behind; a taken-over claim is left alone.
func (s *boltVectorStore) Release(owner int64) error {
	return updateEmbedDB(func(tx *bbolt.Tx) error { return dropClaim(tx, owner) })
}

// embedDBBroken reports an open failure that blames the file's bytes: not a lock timeout, a permission or a missing file.
func embedDBBroken(err error) bool {
	return err != nil && !errors.Is(err, bolterrors.ErrTimeout) && !errors.Is(err, fs.ErrPermission) && !errors.Is(err, os.ErrNotExist)
}

// quarantineEmbedDB moves a damaged or zero-byte embeddings.db aside so the next write open creates a fresh one.
func quarantineEmbedDB() error {
	err := os.Rename(embedDBPath(), embedDBPath()+".damaged")
	if err == nil {
		fmt.Fprintln(os.Stderr, "memory: embeddings.db was damaged; set aside as embeddings.db.damaged, rebuilding empty")
	}
	return err
}

// legacyFormat is the v2 layout FoldLegacy reads.
var legacyFormat = schemas.Format{Name: embedFormat.Name, Version: 2}

// legacyPresent reports whether the file still holds the v2 per-profile buckets; a missing or damaged file has none.
func legacyPresent() (found bool, err error) {
	db, err := openEmbedDB(&bbolt.Options{ReadOnly: true, Timeout: embedReadTimeout})
	if errors.Is(err, os.ErrNotExist) || embedDBBroken(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer db.Close()
	err = db.View(func(tx *bbolt.Tx) error {
		found = tx.Bucket(embedLegacyBucket) != nil
		return nil
	})
	return found, err
}

// FoldLegacy folds the v2 per-profile records embedded with model into the shared cache, once; the rest are dropped.
func (s *boltVectorStore) FoldLegacy(model string) error {
	if found, err := legacyPresent(); err != nil || !found {
		return err
	}
	if err := updateEmbedDB(func(tx *bbolt.Tx) error { return foldLegacyTx(tx, model) }); err != nil {
		return err
	}
	stamps, _ := filepath.Glob(filepath.Join(StateDir(), embedCachePre+"*.refresh*"))
	for _, stamp := range stamps {
		_ = os.Remove(stamp)
	}
	return nil
}

func foldLegacyTx(tx *bbolt.Tx, model string) error {
	old := tx.Bucket(embedLegacyBucket)
	if old == nil { // another process folded first
		return nil
	}
	shared := decodeEmbedRecord(tx)
	if shared.Model != model || shared.Rows == nil {
		shared = vectorRecord{Model: model, Rows: map[string]vectorRow{}}
	}
	_ = old.ForEachBucket(func(name []byte) error { // any profile's vector for a hash is as good as another's
		foldLegacyRecord(&shared, decodeVectorBucket(old.Bucket(name), legacyFormat))
		return nil
	})
	for _, name := range [][]byte{embedLegacyBucket, embedClaimBucket} { // the old buckets and the per-profile claims
		if err := tx.DeleteBucket(name); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucket(embedClaimBucket); err != nil {
		return err
	}
	return putEmbedRecord(tx, shared)
}

// foldLegacyRecord adds rec's rows to shared unless rec is another model or the id is already held.
func foldLegacyRecord(shared *vectorRecord, rec vectorRecord) {
	if rec.Model != shared.Model {
		return
	}
	if shared.Dim == 0 {
		shared.Dim, shared.DimAt = rec.Dim, rec.DimAt
	}
	shared.MintedAt = max(shared.MintedAt, rec.MintedAt)
	for id, row := range rec.Rows {
		if _, held := shared.Rows[id]; !held {
			shared.Rows[id] = row
		}
	}
}

// RefreshDue reads the mint lease and claim lock-free: due when the record is outside its lease and no claim is live.
func (s *boltVectorStore) RefreshDue(now time.Time) bool {
	db, err := openEmbedDB(&bbolt.Options{ReadOnly: true, Timeout: time.Nanosecond})
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return embedDBBroken(err) // the spawned mint quarantines it; a busy or unreadable-by-permission file waits
	}
	defer db.Close()
	due := false
	err = db.View(func(tx *bbolt.Tx) error {
		due = !embedValid(decodeVectorMeta(tx.Bucket(embedVectorBucket), embedFormat), now) && !embedClaimLive(tx, now)
		return nil
	})
	return err == nil && due
}

// embedClaimLive reports whether another worker's unexpired mint claim is recorded.
func embedClaimLive(tx *bbolt.Tx, now time.Time) bool {
	b := tx.Bucket(embedClaimBucket)
	if b == nil {
		return false
	}
	v := b.Get(embedClaimKey)
	return len(v) == 16 && time.Unix(0, int64(binary.BigEndian.Uint64(v))).After(now)
}

// refreshEmbeddings starts a detached mint at most once per claim TTL, whichever workspace it runs through.
func refreshEmbeddings(root, profile string, now time.Time) bool {
	stamp := filepath.Join(StateDir(), "embeddings.refresh")
	if IsUpdateChild() || refreshStampFresh(stamp, now) {
		return false
	}
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return false
	}
	spawned := false
	_, _ = paths.TryFileLock(stamp+".lock", 0, func() error {
		if refreshStampFresh(stamp, now) || !vectorStore.RefreshDue(now) {
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

// refreshStampFresh reports a refresh launched within embedClaimTTL.
func refreshStampFresh(stamp string, now time.Time) bool {
	st, err := os.Stat(stamp)
	return err == nil && now.Sub(st.ModTime()) < embedClaimTTL
}

// spawnEmbedRefresh uses the mint-only index path so automatic refresh never settles the bank.
func spawnEmbedRefresh(root, profile string) bool {
	args := []string{"memory", "index", "--mint-only", "--repo", root}
	return spawnSelf(args, root, "", proc.Values{providers.EnvSessionProfile: profile, UpdateChildEnv: "1"}.Env()...)
}

// RefreshStaleEmbeddings schedules only stale vector mints; no endpoint work runs in the hook.
func RefreshStaleEmbeddings(cwd string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if IsUpdateChild() || !bankExists(cwd) {
		return
	}
	if profile, err := embedEndpoint(context.Background()); err == nil {
		refreshEmbeddings(cwd, profile, time.Now())
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

// cosineHit reuses the minted block hash and ID without constructing an Entry.
func (s *Semantic) cosineHit(h SearchHit) (float64, bool) {
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

// candidates scores the already loaded corpus segments without parsing bank files or constructing entries.
func (s *Semantic) candidates(files []corpusFile, segs []segView, k int, floor float64, skip func(SearchHit) bool) (hits []SearchHit, skipped bool) {
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
	if isSupersededBlock(block) {
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
