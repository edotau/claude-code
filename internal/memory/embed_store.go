package memory

import (
	"container/heap"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/schemas"
)

// The exported aliases let the shadow package implement VectorStore without renaming the memory types.
type (
	VectorRecord = vectorRecord
	VectorRow    = vectorRow
	VectorHead   = vectorHead
	VectorWrite  = vectorWrite
)

// VectorStore is the home of the one shared entry-vector record and its minting claim.
type VectorStore interface {
	// Put merges a mint while releasing its claim; stored is false when the claim was lost or a newer mint landed.
	Put(VectorWrite) (stored bool, err error)
	Vectors() (VectorRecord, error)
	Head() (VectorHead, error)
	// Claim takes the mint claim for owner until expiry; held is the live claim's expiry when another has it.
	Claim(owner int64, now, expiry time.Time) (held time.Time, ok bool, err error)
	// Release drops owner's claim; a taken-over claim is left alone.
	Release(owner int64) error
	// RefreshDue is lock-free: due when the record is outside its lease and no claim is live.
	RefreshDue(now time.Time) bool
	// Present reports whether any record store exists, so a query skips the probe when none does.
	Present() bool
}

// OpenVectorStore is nil when no shadow store is linked; vectors are then absent and nothing can be minted.
var OpenVectorStore func() VectorStore

var errNoVectorStore = errors.New("no embedding store is linked")

type noVectors struct{}

func (noVectors) Put(VectorWrite) (bool, error)  { return false, errNoVectorStore }
func (noVectors) Vectors() (VectorRecord, error) { return VectorRecord{}, nil }
func (noVectors) Head() (VectorHead, error)      { return VectorHead{}, nil }
func (noVectors) Release(int64) error            { return nil }
func (noVectors) RefreshDue(time.Time) bool      { return false }
func (noVectors) Present() bool                  { return false }
func (noVectors) Claim(int64, time.Time, time.Time) (time.Time, bool, error) {
	return time.Time{}, false, errNoVectorStore
}

func vectorStore() VectorStore {
	if OpenVectorStore == nil {
		return noVectors{}
	}
	return OpenVectorStore()
}

// vectorWrite atomically merges a mint while releasing its claim.
type vectorWrite struct {
	Owner   int64
	Started int64
	Merge   func(vectorRecord) vectorRecord
}

const (
	embedValidity   = time.Hour        // a minted record is left alone for this long
	embedOverrideAt = 8 * time.Minute  // --reembed opens in the record's final stretch of the lease
	embedClaimTTL   = 2 * time.Minute  // a minting claim older than this is abandoned
	embedClaimSlack = 30 * time.Second // added to the caller's ctx deadline when sizing a claim
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

// EmbedDBPath is the legacy bbolt cache; the shadow store imports it once and never deletes it.
func EmbedDBPath() string { return embedDBPath() }

// embedDBPath is the legacy bbolt file of the shared entry-vector record, scoped like MemoryStateDir.
func embedDBPath() string { return filepath.Join(paths.MemoryStateDir(), "embeddings.db") }

func mintedTime(f vectorRecord) time.Time { return time.Unix(0, f.MintedAt) }

// embedValid reports whether f was minted less than embedValidity before now; a future MintedAt is not valid.
func embedValid(f vectorRecord, now time.Time) bool {
	return f.MintedAt > 0 && !mintedTime(f).After(now) && now.Sub(mintedTime(f)) < embedValidity
}

// EmbedValid is embedValid for the shadow store's due check.
func EmbedValid(f VectorRecord, now time.Time) bool { return embedValid(f, now) }

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

// readEmbedCache reads the shared record; a missing store or record is empty. A var for tests.
var readEmbedCache = func() (vectorRecord, error) { return vectorStore().Vectors() }

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

// refreshEmbeddings starts a detached mint at most once per claim TTL, whichever workspace it runs through.
func refreshEmbeddings(root, profile string, now time.Time) bool {
	stamp := filepath.Join(paths.MemoryStateDir(), "embeddings.refresh")
	if IsUpdateChild() || refreshStampFresh(stamp, now) {
		return false
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return false
	}
	spawned := false
	_, _ = paths.TryFileLock(stamp+".lock", 0, func() error {
		if refreshStampFresh(stamp, now) || !vectorStore().RefreshDue(now) {
			return nil
		}
		if err := paths.AtomicWriteNoSync(stamp, nil, 0o600); err != nil {
			return err
		}
		spawned = spawn.refresh(root, profile)
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

// RefreshEmbeddingsDetached leaves profile discovery to the throttled mint-only worker.
func RefreshEmbeddingsDetached(cwd string) {
	if !IsUpdateChild() && bankExists() {
		refreshEmbeddings(cwd, "", time.Now())
	}
}

// RefreshStaleEmbeddings schedules only stale vector mints; no endpoint work runs in the hook.
func RefreshStaleEmbeddings(cwd string) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if IsUpdateChild() || !bankExists() {
		return
	}
	if profile, err := embedEndpoint(context.Background()); err == nil {
		refreshEmbeddings(cwd, profile, time.Now())
	}
}

func hitHash(h SearchHit) string {
	d := sha256.New()
	_, _ = io.WriteString(d, h.File)
	_, _ = d.Write([]byte{0})
	_, _ = io.WriteString(d, h.Block)
	var sum [sha256.Size]byte
	var out [2 * sha256.Size]byte
	hex.Encode(out[:], d.Sum(sum[:0]))
	return string(out[:])
}

func newSemantic(query []float32, rows map[string]vectorRow) *semanticQuery {
	s := &semanticQuery{query: query, rows: rows, blocks: make(map[string]string, len(rows))}
	for id, row := range rows {
		if row.BlockHash != "" {
			s.blocks[row.BlockHash] = id
		}
	}
	return s
}

// cosineHit reuses the minted block hash and ID without constructing an Entry.
func (s *semanticQuery) cosineHit(h SearchHit) (float64, bool) {
	id, ok := s.blocks[hitHash(h)]
	if !ok || h.ID != "" && h.ID != id {
		return 0, false
	}
	return s.cosineRow(s.rows[id])
}

func (s *semanticQuery) cosineRow(row vectorRow) (float64, bool) {
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
func (s *semanticQuery) candidates(files []corpusFile, segs []segView, k int, floor float64, skip func(SearchHit) bool) (hits []SearchHit, skipped bool) {
	var all []scoredHit
	for i, seg := range segs {
		if seg == nil || files[i].bank == "archive" || files[i].bank == techdebtDir {
			continue
		}
		for doc := range seg.docs() {
			block, _ := seg.doc(doc)
			h, cos, ok := s.hitFor(files[i], block, floor)
			if !ok {
				continue
			}
			if skip != nil && skip(h) {
				skipped = true
				continue
			}
			all = append(all, scoredHit{hit: h, cos: cos, idx: len(all)})
		}
	}
	for _, a := range topK(all, k) {
		hits = append(hits, a.hit)
	}
	return hits, skipped
}

type scoredHit struct {
	hit SearchHit
	cos float64
	idx int
}

// hitMinHeap keeps the worst hit on top: cos ascending, then arrival index descending.
type hitMinHeap []scoredHit

func (h hitMinHeap) Len() int { return len(h) }
func (h hitMinHeap) Less(i, j int) bool {
	if h[i].cos != h[j].cos {
		return h[i].cos < h[j].cos
	}
	return h[i].idx > h[j].idx
}
func (h hitMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *hitMinHeap) Push(x any)   { *h = append(*h, x.(scoredHit)) }
func (h *hitMinHeap) Pop() any {
	o := *h
	x := o[len(o)-1]
	*h = o[:len(o)-1]
	return x
}

// topK returns the k best hits, cos descending then idx ascending, matching a stable sort truncated to k.
func topK(all []scoredHit, k int) []scoredHit {
	if k <= 0 || len(all) == 0 {
		return nil
	}
	h := make(hitMinHeap, 0, min(k, len(all)))
	for _, a := range all {
		if len(h) < k {
			h = append(h, a)
			if len(h) == k {
				heap.Init(&h)
			}
		} else if a.cos > h[0].cos {
			h[0] = a
			heap.Fix(&h, 0)
		}
	}
	out := []scoredHit(h)
	sort.Slice(out, func(i, j int) bool {
		if out[i].cos != out[j].cos {
			return out[i].cos > out[j].cos
		}
		return out[i].idx < out[j].idx
	})
	return out
}

// hitFor builds the hit for block of file f and its cosine; false for a struck-out or unembedded block at floor.
func (s *semanticQuery) hitFor(f corpusFile, block string, floor float64) (SearchHit, float64, bool) {
	if isSupersededBlock(block) {
		return SearchHit{}, 0, false
	}
	h := SearchHit{Bank: f.bank, File: filepath.Base(f.path), Block: block}
	h.ID = s.blocks[hitHash(h)]
	cos, ok := s.cosineRow(s.rows[h.ID])
	if h.ID == "" || !ok || cos < floor {
		return h, 0, false
	}
	return h, cos, true
}

// embedCacheSchema is the embedding cache's schema version.
const embedCacheSchema = 3

// EmbedCacheSchema is the version stamped on every stored record.
const EmbedCacheSchema = embedCacheSchema

var _ = schemas.Register(schemas.Format{
	Name: "memory-embeddings", Owner: "internal/memory", Kind: schemas.Store, Version: embedCacheSchema,
	Glob: embedDBPath,
	Keep: "bbolt metadata and binary float32 rows of one shared cache; a version mismatch is a miss",
})

// vectorHead is a record's metadata without its rows; Rows says some are recorded.
type vectorHead struct {
	Model    string
	MintedAt int64
	Rows     bool
}
