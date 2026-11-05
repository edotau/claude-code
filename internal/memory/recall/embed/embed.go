package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// embedCacheSchema is the embedding cache's schema version; a mismatch is a miss, never migrated.
const embedCacheSchema = 2

const (
	embedBatch     = 16
	embedTextRunes = 2000
	embedMaxRows   = 4000
	embedBodyLimit = 32 << 20
	embedCachePre  = "embeddings-"
	embedBackoff   = time.Minute // after a failed query, keyword-only for this long
)

// embedRecallWait is the hard budget for the per-prompt embedding, credential included; tests shorten it.
var embedRecallWait = 1200 * time.Millisecond

// SetEmbedRecallWait overrides the per-prompt embedding budget and returns the restore func (tests only).
func SetEmbedRecallWait(d time.Duration) (restore func()) {
	prev := embedRecallWait
	embedRecallWait = d
	return func() { embedRecallWait = prev }
}

var embedFormat = schemas.Register(schemas.Format{
	Name: "memory-embeddings", Owner: "internal/memory", Kind: schemas.Store, Version: embedCacheSchema,
	Glob: embedDBPath,
	Keep: "bbolt profile metadata and binary float32 rows; a version mismatch is a miss",
})

var safeProfile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// vectorRecord holds content vectors scoped to a profile, workspace and model.
type vectorRecord struct {
	Schema    int                  `json:"schema"`
	Namespace string               `json:"namespace"`
	Model     string               `json:"model"`
	Dim       int                  `json:"dim,omitempty"`       // vector dimension of the endpoint's latest response
	DimAt     int64                `json:"dim_at,omitempty"`    // ns of the run that set Dim
	MintedAt  int64                `json:"minted_at,omitempty"` // ns of the last full mint
	Rows      map[string]vectorRow `json:"rows"`
}

type vectorRow struct {
	Root      string `json:"root,omitempty"` // bank root whose index wrote the row; pruning never crosses roots
	Hash      string `json:"hash"`           // sha256 of the embedded text
	Vec       string `json:"-"`              // binary little-endian float32, L2-normalised
	BlockHash string `json:"block_hash,omitempty"`
}

// EmbedClient embeds texts with one named model; Embed honours ctx.
type EmbedClient struct {
	Profile   string // the profile whose cache this client reads and writes
	Model     string
	Namespace string // profile|host|model: vectors are comparable only within one namespace
	EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)
}

// embedder turns text batches into embedding vectors.
type embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

func (c *EmbedClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return c.EmbedFunc(ctx, texts)
}

// OpenEmbedClient builds a model client; an empty profile uses the agent record.
var OpenEmbedClient = openDatabricksEmbed

// entryEmbedText is what an entry is embedded as: its type and body, capped.
func entryEmbedText(e bank.Entry) string {
	r := []rune(e.Type + ": " + e.Body)
	if len(r) > embedTextRunes {
		r = r[:embedTextRunes]
	}
	return string(r)
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// freshEmbedCache is an empty cache for c's namespace.
func freshEmbedCache(c *EmbedClient) vectorRecord {
	return vectorRecord{Namespace: c.Namespace, Model: c.Model, Rows: map[string]vectorRow{}}
}

// currentEntryHashes is the content hash of each embeddable entry in root's corpus right now.
func currentEntryHashes(root string) map[string]string {
	out := map[string]string{}
	for _, h := range bank.CorpusBlocks(root) {
		e := bank.EntryFromHit(h)
		if e.Archived || e.Superseded {
			continue
		}
		if _, dup := out[e.ID]; !dup {
			out[e.ID] = textHash(entryEmbedText(e))
		}
	}
	return out
}

// EmbedEntries embeds changed live entries; a valid lease returns ErrEmbedValid without minting.
func EmbedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, false)
}

// ReembedEntries re-embeds every live entry once the override window opens, otherwise returning ErrReembedEarly.
func ReembedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, true)
}

// checkEmbedLease is nil when a mint may start on rec: expired or missing, or force inside the override window.
func checkEmbedLease(rec vectorRecord, now time.Time, force bool) error {
	if force {
		if at, open := reembedOpensAt(rec, now); !open {
			return fmt.Errorf("%w: opens at %s", ErrReembedEarly, at.Format("15:04"))
		}
		return nil
	}
	if embedValid(rec, now) {
		return fmt.Errorf("%w until %s", ErrEmbedValid, MintedTime(rec).Add(embedValidity).Format("15:04"))
	}
	return nil
}

// embedBase is the cache a mint starts from: c's record, or a fresh one when forced or from another namespace.
func embedBase(c *EmbedClient, now time.Time, force bool) (vectorRecord, error) {
	if !SafeProfile(c.Profile) {
		return vectorRecord{}, fmt.Errorf("embeddings: unsafe profile name %q", c.Profile)
	}
	rec, err := readEmbedCache(c.Profile)
	if err != nil {
		return vectorRecord{}, err
	}
	if err := checkEmbedLease(rec, now, force); err != nil {
		return vectorRecord{}, err
	}
	if force || rec.Namespace != c.Namespace || rec.Rows == nil {
		rec = freshEmbedCache(c)
	}
	return rec, nil
}

func embedEntries(ctx context.Context, root string, c *EmbedClient, force bool) (embedded, total int, err error) {
	now := time.Now()
	rec, err := embedBase(c, now, force)
	if err != nil {
		return 0, 0, err
	}
	ids, texts, hashes, blocks, total := pendingEmbeds(root, rec)
	started := now.UnixNano()
	owner, err := newEmbedOwner()
	if err != nil {
		return 0, total, err
	}
	held, ok, err := vectorStore.Claim(c.Profile, owner, now, embedClaimExpiry(ctx, now))
	if err != nil {
		return 0, total, err
	}
	if !ok {
		return 0, total, fmt.Errorf("%w until %s", ErrEmbedBusy, held.Format("15:04"))
	}
	run := &embedRun{c: c, root: root, force: force, fresh: map[string]vectorRow{}, started: started}
	embedded, embedErr := run.embed(ctx, c, ids, texts, hashes, blocks)
	run.current = currentEntryHashes(root)
	stored, cerr := run.store(owner, embedErr == nil)
	if !stored && cerr == nil {
		embedded = 0
		cerr = ErrEmbedSuperseded
	}
	return embedded, total, errors.Join(embedErr, cerr)
}

// store merges the run into the current record under owner's claim; complete starts the lease, a partial run does not.
func (r *embedRun) store(owner int64, complete bool) (stored bool, err error) {
	stored, err = vectorStore.Put(VectorWrite{Profile: r.c.Profile, Owner: owner, Started: r.started, Merge: func(cur vectorRecord) vectorRecord {
		out := r.merge(cur)
		if complete { // a partial run keeps its paid-for rows but does not start the lease
			out.MintedAt = time.Now().UnixNano()
		}
		return out
	}})
	if err != nil {
		vectorStore.Release(r.c.Profile, owner) // the tx rolled back, so the claim is still ours
	}
	return stored, err
}

// cacheRowCurrent reports whether row matches the entry's text and block hashes and decodes to the cache's dimension.
func cacheRowCurrent(row vectorRow, hash, blockHash string, cacheDim int) bool {
	if row.Hash != hash || row.BlockHash != blockHash {
		return false
	}
	v, decoded := decodeVec(row.Vec)
	return decoded && (cacheDim == 0 || len(v) == cacheDim)
}

// pendingEmbeds lists root's live entries whose vector in cache is missing, stale or undecodable, and the live count.
func pendingEmbeds(root string, cache vectorRecord) (ids, texts, hashes, blocks []string, total int) {
	live := map[string]bool{}
	cacheDim := cacheDimOf(cache)
	for _, h := range bank.CorpusBlocks(root) {
		e := bank.EntryFromHit(h)
		if e.Archived || e.Superseded || live[e.ID] { // the first of a duplicated id wins
			continue
		}
		text := entryEmbedText(e)
		hash := textHash(text)
		live[e.ID] = true
		if cacheRowCurrent(cache.Rows[e.ID], hash, hitHash(h), cacheDim) {
			continue
		}
		ids, texts, hashes = append(ids, e.ID), append(texts, text), append(hashes, hash)
		blocks = append(blocks, hitHash(h))
	}
	return ids, texts, hashes, blocks, len(live)
}

// embedRun is one mint's paid-for rows, merged into the record that is current at commit time.
type embedRun struct {
	c        *EmbedClient
	root     string
	force    bool // override: fresh rows replace the old ones even when their hash matches
	fresh    map[string]vectorRow
	freshDim int
	started  int64
	current  map[string]string // content hash of each embeddable entry in root right now
}

func (r *embedRun) merge(cur vectorRecord) vectorRecord {
	if cur.Namespace != r.c.Namespace || cur.Rows == nil {
		cur = freshEmbedCache(r.c)
	}
	fresh := r.fresh
	if r.freshDim > 0 && cur.Dim != r.freshDim {
		if cur.DimAt > r.started { // a newer run already moved the dimension
			fresh = nil
		} else {
			r.moveDim(&cur)
		}
	}
	r.absorb(cur, fresh)
	r.shed(cur)
	return cur
}

// absorb folds fresh rows for still-current entries into cur; an equal-hash, same-dimension row keeps its vector.
func (r *embedRun) absorb(cur vectorRecord, fresh map[string]vectorRow) {
	for id, row := range fresh {
		if want, ok := r.current[id]; !ok || row.Hash != want {
			continue
		}
		if old, have := cur.Rows[id]; have && !r.force && old.Hash == row.Hash && sameDim(old, row) {
			old.BlockHash = row.BlockHash
			cur.Rows[id] = old
			continue
		}
		cur.Rows[id] = row
	}
}

// moveDim handles the endpoint's dimension moving: drop vectors it cannot match and record the new dimension.
func (r *embedRun) moveDim(cur *vectorRecord) {
	for id, row := range cur.Rows {
		if v, ok := decodeVec(row.Vec); ok && len(v) > 0 && len(v) != r.freshDim {
			delete(cur.Rows, id)
		}
	}
	cur.Dim, cur.DimAt = r.freshDim, r.started
}

func sameDim(a, b vectorRow) bool {
	av, aok := decodeVec(a.Vec)
	bv, bok := decodeVec(b.Vec)
	return aok && bok && len(av) == len(bv)
}

// shed drops this root's rows whose entry is gone once the root holds more than embedMaxRows.
func (r *embedRun) shed(cur vectorRecord) {
	own := 0
	for _, row := range cur.Rows {
		if row.Root == r.root {
			own++
		}
	}
	if own <= embedMaxRows {
		return
	}
	for id, row := range cur.Rows {
		if _, ok := r.current[id]; row.Root == r.root && !ok {
			delete(cur.Rows, id)
		}
	}
}

// Semantic scores entries against one embedded query, from the cached entry vectors.
type Semantic struct {
	query  []float32
	rows   map[string]vectorRow
	blocks map[string]string
}

// ReadProfileCache is the per-profile read NewSemantic uses; a var so tests can fail one profile.
var ReadProfileCache = readEmbedCache

// NewSemantic embeds a query against the session cache, falling back to the agent record.
func NewSemantic(query string) (*Semantic, error) {
	if os.Getenv("CLAUDE_RECALL_SEMANTIC") == "0" {
		return nil, errors.New("disabled by CLAUDE_RECALL_SEMANTIC=0")
	}
	profile, cache, err := semanticCache()
	if err != nil {
		return nil, err
	}
	attempt := time.Now()
	backoff := filepath.Join(memstore.StateDir(), embedCachePre+profile+".backoff")
	if err := backoffPause(backoff); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedRecallWait)
	defer cancel()
	done := make(chan semanticResult, 1)
	go func() { done <- querySemantic(ctx, profile, query, cache) }()
	select {
	case r := <-done:
		if r.err != nil {
			_ = paths.AtomicWrite(backoff, nil, 0o600)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("query embedding exceeded %s", embedRecallWait)
			}
			return nil, r.err
		}
		clearOldBackoff(backoff, attempt)
		return newSemantic(r.vec, cache.Rows), nil
	case <-ctx.Done():
		_ = paths.AtomicWrite(backoff, nil, 0o600) // a slow endpoint costs one prompt the full wait, not every prompt
		return nil, fmt.Errorf("query embedding exceeded %s", embedRecallWait)
	}
}

// backoffPause is an error naming when semantic resumes while a backoff marker is still fresh.
func backoffPause(backoff string) error {
	if st, err := os.Stat(backoff); err == nil && time.Since(st.ModTime()) < embedBackoff {
		return fmt.Errorf("endpoint failed recently; semantic paused until %s", st.ModTime().Add(embedBackoff).Format("15:04"))
	}
	return nil
}

// clearOldBackoff removes a backoff marker written before attempt began, since the endpoint just answered.
func clearOldBackoff(backoff string, attempt time.Time) {
	if st, err := os.Stat(backoff); err == nil && st.ModTime().Before(attempt) {
		_ = os.Remove(backoff)
	}
}

// Cosine is e's similarity to the query; false when e has no vector for its current content.
func (s *Semantic) Cosine(e bank.Entry) (float64, bool) {
	row, ok := s.rows[e.ID]
	if !ok || row.Hash != textHash(entryEmbedText(e)) {
		return 0, false
	}
	return s.cosineRow(row)
}

// dominantDim returns the most frequent decoded vector dimension, with ties going to the larger dimension.
func dominantDim(rows map[string]vectorRow) int {
	counts := map[int]int{}
	for _, r := range rows {
		if v, ok := decodeVec(r.Vec); ok {
			counts[len(v)]++
		}
	}
	best, frequency := 0, 0
	for dim, n := range counts {
		if n > frequency || n == frequency && dim > best {
			best, frequency = dim, n
		}
	}
	return best
}

// cacheDimOf is the endpoint dimension recorded in the cache, else the dominant row dimension.
func cacheDimOf(c vectorRecord) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return dominantDim(c.Rows)
}

// cacheDimMatches reports whether the cache's vector dimension is dim.
func cacheDimMatches(c vectorRecord, dim int) bool {
	cacheDim := cacheDimOf(c)
	return cacheDim > 0 && cacheDim == dim
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	n := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * n
	}
	return out
}

func encodeVec(v []float32) string {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return string(b)
}

func decodeVec(s string) ([]float32, bool) {
	b := []byte(s)
	if len(b)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, true
}

// pickEmbeddingModel chooses a ready embeddings endpoint from a live catalog, preferring gte then bge.
func pickEmbeddingModel(infos []providers.ModelInfo) (string, bool) {
	var ids []string
	for _, m := range infos {
		if strings.Contains(m.Task, "embed") && (m.Ready == "" || m.Ready == "READY") {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	for _, want := range []string{"gte-large", "bge-large", "gte", "bge"} {
		for _, id := range ids {
			if strings.Contains(id, want) {
				return id, true
			}
		}
	}
	if len(ids) > 0 {
		return ids[0], true
	}
	return "", false
}

// PostEmbeddings POSTs {"input": texts} and decodes the OpenAI-shaped {"data":[{"index","embedding"}]} reply.
func PostEmbeddings(ctx context.Context, url string, texts []string, authorize func(http.Header) error) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := authorize(req.Header); err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, embedBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: %s", resp.Status)
	}
	return decodeEmbeddings(raw)
}

// decodeEmbeddings places each reply vector at its index; a gap, bad index or empty vector is malformed.
func decodeEmbeddings(raw []byte) ([][]float32, error) {
	var doc struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("embeddings: decode: %w", err)
	}
	out := make([][]float32, len(doc.Data))
	for _, d := range doc.Data {
		if d.Index < 0 || d.Index >= len(out) || len(d.Embedding) == 0 {
			return nil, errors.New("embeddings: malformed reply")
		}
		out[d.Index] = d.Embedding
	}
	for _, v := range out {
		if v == nil {
			return nil, errors.New("embeddings: malformed reply")
		}
	}
	return out, nil
}

func (r *embedRun) embed(ctx context.Context, client embedder, ids, texts, hashes, blocks []string) (embedded int, embedErr error) {
	for i := 0; i < len(texts) && embedErr == nil; i += embedBatch { // network calls stay outside any transaction
		j := min(i+embedBatch, len(texts))
		vecs, err := client.Embed(ctx, texts[i:j])
		if err == nil && len(vecs) != j-i {
			err = fmt.Errorf("embedding endpoint returned %d vectors for %d texts", len(vecs), j-i)
		}
		if err != nil {
			embedErr = err
			break
		}
		for k, v := range vecs {
			r.freshDim = len(v)
			r.fresh[ids[i+k]] = vectorRow{Root: r.root, Hash: hashes[i+k], BlockHash: blocks[i+k], Vec: encodeVec(normalize(v))}
		}
		embedded += j - i
	}
	return embedded, embedErr
}

func semanticCache() (string, vectorRecord, error) {
	var profile string
	var cache vectorRecord
	var readErr error
	for _, name := range []string{providers.SessionProfile(), providers.AgentRecord("claude").Profile} {
		if name == "" || name == profile || !SafeProfile(name) {
			continue
		}
		c, err := ReadProfileCache(name)
		if err != nil { // a locked or unreadable profile is a miss; the next one may serve
			readErr = err
			continue
		}
		if c.Model != "" && len(c.Rows) > 0 {
			profile, cache = name, c
			break
		}
	}
	if profile == "" {
		if readErr != nil {
			return "", vectorRecord{}, fmt.Errorf("embedding cache unreadable: %w", readErr)
		}
		return "", vectorRecord{}, errors.New("no embedding cache")
	}
	return profile, cache, nil
}

type semanticResult struct {
	vec []float32
	err error
}

func querySemantic(ctx context.Context, profile, query string, cache vectorRecord) semanticResult {
	c, err := OpenEmbedClient(ctx, profile, cache.Model)
	if err != nil {
		return semanticResult{err: err}
	}
	if cache.Namespace == "" || c.Namespace != cache.Namespace { // checked first: no prompt text leaves on a mismatch
		return semanticResult{err: errors.New("embedding cache belongs to another workspace or model")}
	}
	vecs, err := c.Embed(ctx, []string{query})
	if err == nil && len(vecs) != 1 {
		err = errors.New("embedding endpoint returned no query vector")
	}
	if err != nil {
		return semanticResult{err: err}
	}
	if !cacheDimMatches(cache, len(vecs[0])) {
		return semanticResult{err: errors.New("embedding dimension differs from the cache")}
	}
	return semanticResult{vec: normalize(vecs[0])}
}
