package memory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"go.etcd.io/bbolt"
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

func (c *EmbedClient) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return c.EmbedFunc(ctx, texts)
}

// openEmbedClient builds a model client; an empty profile uses the agent record.
var openEmbedClient = openDatabricksEmbed

// entryEmbedText is what an entry is embedded as: its type and body, capped.
func entryEmbedText(e Entry) string {
	return textutil.TruncateRunes(e.Type+": "+e.Body, embedTextRunes)
}

// freshEmbedCache is an empty cache for c's namespace.
func freshEmbedCache(c *EmbedClient) vectorRecord {
	return vectorRecord{Namespace: c.Namespace, Model: c.Model, Rows: map[string]vectorRow{}}
}

// currentEntryHashes is the content hash of each embeddable entry in root's corpus right now.
func currentEntryHashes(root string) (map[string]string, error) {
	hits, err := corpusBlocks(root)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, h := range hits {
		e := entryFromHit(h)
		if e.Archived || e.Superseded {
			continue
		}
		if _, dup := out[e.ID]; !dup {
			out[e.ID] = textutil.SHA256Hex([]byte(entryEmbedText(e)))
		}
	}
	return out, nil
}

// embedChanged embeds changed live entries; a valid lease returns errEmbedValid without minting.
func embedChanged(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, false)
}

// reembedEntries re-embeds every live entry once the override window opens, otherwise returning errReembedEarly.
func reembedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, true)
}

// checkEmbedLease is nil when a mint may start on rec: expired or missing, or force inside the override window.
func checkEmbedLease(rec vectorRecord, now time.Time, force bool) error {
	if force {
		if at, open := reembedOpensAt(rec, now); !open {
			return fmt.Errorf("%w: opens at %s", errReembedEarly, at.Format("15:04"))
		}
		return nil
	}
	if embedValid(rec, now) {
		return fmt.Errorf("%w until %s", errEmbedValid, mintedTime(rec).Add(embedValidity).Format("15:04"))
	}
	return nil
}

// embedBase is the cache a mint starts from: c's record, or a fresh one when forced or from another namespace.
func embedBase(c *EmbedClient, now time.Time, force bool) (vectorRecord, error) {
	if !safeProfile.MatchString(c.Profile) {
		return vectorRecord{}, fmt.Errorf("embeddings: unsafe profile name %q", c.Profile)
	}
	rec, err := readEmbedCache(c.Profile)
	if embedDBBroken(err) && quarantineEmbedDB() == nil { // a derived cache: set the damaged file aside and mint anew
		rec, err = readEmbedCache(c.Profile)
	}
	if err != nil {
		return vectorRecord{}, err
	}
	if rec.Namespace != c.Namespace || rec.Rows == nil { // a foreign record's lease does not bind this namespace
		rec = freshEmbedCache(c)
	}
	if err := checkEmbedLease(rec, now, force); err != nil {
		return vectorRecord{}, err
	}
	if force {
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
	ids, texts, hashes, blocks, total, err := pendingEmbeds(root, rec)
	if err != nil {
		return 0, 0, err
	}
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
		return 0, total, fmt.Errorf("%w until %s", errEmbedBusy, held.Format("15:04"))
	}
	if _, err := embedBase(c, now, force); err != nil { // a mint that landed before our claim must not be paid for twice
		return 0, total, errors.Join(err, vectorStore.Release(c.Profile, owner))
	}
	run := &embedRun{c: c, root: root, force: force, fresh: map[string]vectorRow{}, started: started}
	embedded, embedErr := run.embed(ctx, ids, texts, hashes, blocks)
	current, cerr := currentEntryHashes(root)
	if cerr != nil {
		rerr := vectorStore.Release(c.Profile, owner) // nothing was stored, so the claim is still ours to drop
		return embedded, total, errors.Join(embedErr, cerr, rerr)
	}
	run.current = current
	stored, cerr := run.store(owner, embedErr == nil)
	if !stored && cerr == nil {
		embedded = 0
		cerr = errEmbedSuperseded
	}
	return embedded, total, errors.Join(embedErr, cerr)
}

// store merges the run into the current record under owner's claim; complete starts the lease, a partial run does not.
func (r *embedRun) store(owner int64, complete bool) (stored bool, err error) {
	stored, err = vectorStore.Put(vectorWrite{Profile: r.c.Profile, Owner: owner, Started: r.started, Merge: func(cur vectorRecord) vectorRecord {
		out := r.merge(cur)
		if complete && r.covers(out) { // a partial or migrated-incomplete run keeps its rows but does not start the lease
			out.MintedAt = time.Now().UnixNano()
		}
		return out
	}})
	if err != nil {
		err = errors.Join(err, vectorStore.Release(r.c.Profile, owner)) // the tx rolled back, so the claim is still ours
	}
	return stored, err
}

// cacheRowCurrent reports whether row matches the entry's text and block hashes and decodes to the cache's dimension.
func cacheRowCurrent(row vectorRow, hash, blockHash string, cacheDim int) bool {
	if row.Hash != hash || row.BlockHash != blockHash {
		return false
	}
	dim, ok := vecDim(row.Vec)
	return ok && (cacheDim == 0 || dim == cacheDim)
}

// pendingEmbeds lists root's live entries whose vector in cache is missing, stale or undecodable, and the live count.
func pendingEmbeds(root string, cache vectorRecord) (ids, texts, hashes, blocks []string, total int, err error) {
	live := map[string]bool{}
	cacheDim := cacheDimOf(cache)
	hits, err := corpusBlocks(root)
	if err != nil {
		return nil, nil, nil, nil, 0, err
	}
	for _, h := range hits {
		e := entryFromHit(h)
		if e.Archived || e.Superseded || live[e.ID] { // the first of a duplicated id wins
			continue
		}
		text := entryEmbedText(e)
		hash := textutil.SHA256Hex([]byte(text))
		live[e.ID] = true
		if cacheRowCurrent(cache.Rows[e.ID], hash, hitHash(h), cacheDim) {
			continue
		}
		ids, texts, hashes = append(ids, e.ID), append(texts, text), append(hashes, hash)
		blocks = append(blocks, hitHash(h))
	}
	return ids, texts, hashes, blocks, len(live), nil
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

// covers reports whether rec holds a row at the current hash for every live entry in the run's corpus.
func (r *embedRun) covers(rec vectorRecord) bool {
	for id, hash := range r.current {
		if row, ok := rec.Rows[id]; !ok || row.Hash != hash {
			return false
		}
	}
	return true
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
		if dim, ok := vecDim(row.Vec); ok && dim > 0 && dim != r.freshDim {
			delete(cur.Rows, id)
		}
	}
	cur.Dim, cur.DimAt = r.freshDim, r.started
}

func sameDim(a, b vectorRow) bool {
	ad, aok := vecDim(a.Vec)
	bd, bok := vecDim(b.Vec)
	return aok && bok && ad == bd
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

// readProfileCache is the per-profile read NewSemantic uses; a var so tests can fail one profile.
var readProfileCache = readEmbedCache

// NewSemantic embeds a query against the session cache, falling back to the agent record.
func NewSemantic(query string) (*Semantic, error) {
	if os.Getenv("CLAUDE_RECALL_SEMANTIC") == "0" {
		return nil, errors.New("disabled by CLAUDE_RECALL_SEMANTIC=0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedRecallWait)
	defer cancel()
	profile, cache, err := semanticCache(ctx)
	if err != nil {
		return nil, err
	}
	attempt := time.Now()
	backoff := embedBackoffPath(profile)
	done := make(chan semanticResult, 1)
	go func() { done <- querySemantic(ctx, profile, query, cache) }()
	select {
	case r := <-done:
		if r.err != nil {
			if !errors.Is(r.err, errEmbedMismatch) { // a local config mismatch says nothing about the endpoint
				_ = paths.AtomicWrite(backoff, nil, 0o600)
			}
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

// dominantDim returns the most frequent decoded vector dimension, with ties going to the larger dimension.
func dominantDim(rows map[string]vectorRow) int {
	counts := map[int]int{}
	for _, r := range rows {
		if dim, ok := vecDim(r.Vec); ok {
			counts[dim]++
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

// vecDim is the dimension of an encoded vector; false when its length is not a whole number of float32s.
func vecDim(s string) (int, bool) { return len(s) / 4, len(s)%4 == 0 }

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

// postEmbeddings POSTs {"input": texts} and decodes the OpenAI-shaped {"data":[{"index","embedding"}]} reply.
func postEmbeddings(ctx context.Context, url string, texts []string, authorize func(http.Header) error) ([][]float32, error) {
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

func (r *embedRun) embed(ctx context.Context, ids, texts, hashes, blocks []string) (embedded int, embedErr error) {
	for i := 0; i < len(texts) && embedErr == nil; i += embedBatch { // network calls stay outside any transaction
		j := min(i+embedBatch, len(texts))
		vecs, err := r.c.Embed(ctx, texts[i:j])
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

// embedProfiles yields the session profile, then the claude record's when it differs; the record loads only if reached.
func embedProfiles(ctx context.Context) iter.Seq[string] {
	return func(yield func(string) bool) {
		name := providers.SessionProfile()
		if name != "" && !yield(name) {
			return
		}
		if other := claudeRecordProfile(ctx); other != "" && other != name {
			yield(other)
		}
	}
}

// claudeRecordProfile reads the vault record within ctx; the abandoned read ends on the vault's own timeout.
func claudeRecordProfile(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}
	done := make(chan string, 1)
	go func() { done <- providers.AgentRecord("claude").Profile }()
	select {
	case p := <-done:
		return p
	case <-ctx.Done():
		return ""
	}
}

func embedBackoffPath(profile string) string {
	return filepath.Join(StateDir(), embedCachePre+profile+".backoff")
}

// hasCacheMeta reports whether profile's metadata shows a cache, within ctx's deadline; it reads no vectors.
func hasCacheMeta(ctx context.Context, profile string) (bool, error) {
	timeout := embedReadTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(dl))
	}
	if timeout <= 0 {
		return false, ctx.Err()
	}
	db, err := bbolt.Open(embedDBPath(), 0o600, &bbolt.Options{ReadOnly: true, Timeout: timeout})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer db.Close()
	var ok bool
	err = db.View(func(tx *bbolt.Tx) error {
		m := decodeEmbedMetadata(tx, profile)
		ok = m.Model != "" && len(m.Rows) > 0
		return nil
	})
	return ok, err
}

// readCacheWithin is readProfileCache bounded by ctx; the abandoned read ends on its own lock timeout.
func readCacheWithin(ctx context.Context, profile string) (vectorRecord, error) {
	type read struct {
		c   vectorRecord
		err error
	}
	done := make(chan read, 1)
	go func() {
		c, err := readProfileCache(profile)
		done <- read{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		return vectorRecord{}, ctx.Err()
	}
}

// profileCache loads profile's vectors when its metadata shows a cache; paused means its backoff marker is fresh.
func profileCache(ctx context.Context, profile string) (c vectorRecord, paused bool, err error) {
	if ok, err := hasCacheMeta(ctx, profile); err != nil || !ok {
		return vectorRecord{}, false, err
	}
	if err := backoffPause(embedBackoffPath(profile)); err != nil {
		return vectorRecord{}, true, err
	}
	c, err = readCacheWithin(ctx, profile)
	return c, false, err
}

// semanticCache picks the first unpaused profile with a cache; a backoff marker skips a profile before any vector loads.
func semanticCache(ctx context.Context) (string, vectorRecord, error) {
	var readErr, pauseErr error
	for name := range embedProfiles(ctx) { // lazy: the claude record's profile loads only if the session one misses
		if !safeProfile.MatchString(name) {
			continue
		}
		c, paused, err := profileCache(ctx, name)
		if paused { // this profile's endpoint is backing off; the next one may still serve
			pauseErr = err
			continue
		}
		if err != nil { // a locked or unreadable profile is a miss; the next one may serve
			readErr = err
			continue
		}
		if c.Model != "" && len(c.Rows) > 0 {
			return name, c, nil
		}
	}
	if pauseErr != nil {
		return "", vectorRecord{}, pauseErr
	}
	if ctx.Err() != nil {
		return "", vectorRecord{}, fmt.Errorf("query embedding exceeded %s", embedRecallWait)
	}
	return "", vectorRecord{}, semanticCacheMiss(readErr)
}

func semanticCacheMiss(readErr error) error {
	if readErr != nil {
		return fmt.Errorf("embedding cache unreadable: %w", readErr)
	}
	return errors.New("no embedding cache")
}

// errEmbedMismatch marks a query that cannot use the cache (namespace or dimension): local config, not an endpoint failure.
var errEmbedMismatch = errors.New("embedding cache mismatch")

type semanticResult struct {
	vec []float32
	err error
}

func querySemantic(ctx context.Context, profile, query string, cache vectorRecord) semanticResult {
	c, err := openEmbedClient(ctx, profile, cache.Model)
	if err != nil {
		return semanticResult{err: err}
	}
	if cache.Namespace == "" || c.Namespace != cache.Namespace { // checked first: no prompt text leaves on a mismatch
		return semanticResult{err: fmt.Errorf("%w: belongs to another workspace or model", errEmbedMismatch)}
	}
	vecs, err := c.Embed(ctx, []string{query})
	if err == nil && len(vecs) != 1 {
		err = errors.New("embedding endpoint returned no query vector")
	}
	if err != nil {
		return semanticResult{err: err}
	}
	if !cacheDimMatches(cache, len(vecs[0])) {
		return semanticResult{err: fmt.Errorf("%w: dimension differs", errEmbedMismatch)}
	}
	return semanticResult{vec: normalize(vecs[0])}
}

// routerLink reads the running loopback router address and client secret without starting it.
var routerLink = func(ctx context.Context) (base, secret string, ok bool) {
	st, up := gateway.Running(ctx)
	if !up {
		return "", "", false
	}
	secret, err := gateway.ClientSecret()
	return gateway.ViaBase(gateway.Base(st.Port)), secret, err == nil
}

// embeddingModelFor picks p's embeddings endpoint from the models it serves.
func embeddingModelFor(ctx context.Context, p *providers.Provider) (string, error) {
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return "", err
	}
	model, ok := pickEmbeddingModel(infos)
	if !ok {
		return "", fmt.Errorf("profile %s serves no embeddings endpoint", p.Name)
	}
	return model, nil
}

// embedNamespace names the workspace and model a cache was embedded against.
func embedNamespace(name, host, model string) string {
	return name + "|" + strings.TrimRight(host, "/") + "|" + model
}

// openDatabricksEmbed is profile's embeddings client; credentials come only from providers.Authorize.
func openDatabricksEmbed(ctx context.Context, profile, model string) (*EmbedClient, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	sel, err := providers.Select(reg, profile) // "" = the agent record, then the registry default
	if err != nil {
		return nil, err
	}
	p := sel.Provider
	if p.Preset != providers.PresetDatabricks || p.Host == "" {
		return nil, fmt.Errorf("profile %s is not a databricks workspace", p.Name)
	}
	if model == "" {
		if model, err = embeddingModelFor(ctx, p); err != nil {
			return nil, err
		}
	}
	ns := embedNamespace(p.Name, p.Host, model)
	return &EmbedClient{Profile: p.Name, Model: model, Namespace: ns, EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		return embedDatabricks(ctx, p, model, texts)
	}}, nil
}

func embedDatabricks(ctx context.Context, p *providers.Provider, model string, texts []string) ([][]float32, error) {
	url := strings.TrimRight(p.Host, "/") + "/serving-endpoints/" + model + "/invocations"
	if base, secret, ok := routerLink(ctx); ok { // the daemon holds the warm credential and upstream connections
		via := base + "/p/" + p.Name + "/" + providers.ProtocolOpenAI + "/" + model + "/invocations"
		vecs, err := postEmbeddings(ctx, via, texts, func(h http.Header) error { h.Set(gateway.ClientHeader, secret); return nil })
		if err == nil || ctx.Err() != nil {
			return vecs, err
		}
	}
	return postEmbeddings(ctx, url, texts, func(h http.Header) error { return providers.Authorize(ctx, p, h) })
}
