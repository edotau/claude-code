package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/schemas"
)

// embedCacheSchema is the embedding cache's schema version; a mismatch is a miss, never migrated.
const embedCacheSchema = 1

const (
	embedBatch     = 16
	embedTextRunes = 2000
	embedMaxRows   = 4000
	embedBodyLimit = 32 << 20
	embedCachePre  = "embeddings-"
	embedBackoff   = 5 * time.Minute // after a timed-out query, keyword-only for this long
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
	Glob: EmbedDBPath,
	Keep: "bbolt cache, one JSON record per profile; a version mismatch is a miss",
})

var safeProfile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// embedCacheFile maps an entry id to the vector of its content; Namespace (profile|host|model) pins which
// workspace's endpoint wrote it, since identically named endpoints on different workspaces are different models.
type embedCacheFile struct {
	Schema    int                 `json:"schema"`
	Namespace string              `json:"namespace"`
	Model     string              `json:"model"`
	Dim       int                 `json:"dim,omitempty"`       // vector dimension of the endpoint's latest response
	DimAt     int64               `json:"dim_at,omitempty"`    // start time (ns) of the run that set Dim; older runs cannot move it
	MintedAt  int64               `json:"minted_at,omitempty"` // unix ns of the last full mint; the validity lease runs from it
	Rows      map[string]embedRow `json:"rows"`
}

type embedRow struct {
	Root string `json:"root,omitempty"` // bank root whose index wrote the row; pruning never crosses roots
	Hash string `json:"hash"`           // sha256 of the embedded text
	Vec  string `json:"vec"`            // base64 little-endian float32, L2-normalised
}

// EmbedClient embeds texts with one named model; Embed honours ctx.
type EmbedClient struct {
	Profile   string // the profile whose cache this client reads and writes
	Model     string
	Namespace string // profile|host|model: vectors are comparable only within one namespace
	Embed     func(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenEmbedClient builds profile's client for model ("" = discover one; profile "" = the pin); a var so tests fake it.
var OpenEmbedClient = openDatabricksEmbed

// routerLink is the running loopback router's base URL and client secret; ok is false when it is down (never started here).
var routerLink = func(ctx context.Context) (base, secret string, ok bool) {
	st, up := proxy.Running(ctx)
	if !up {
		return "", "", false
	}
	secret, err := proxy.ClientSecret()
	return proxy.Base(st.Port), secret, err == nil
}

// entryEmbedText is what an entry is embedded as: its type and body, capped.
func entryEmbedText(e Entry) string {
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
func freshEmbedCache(c *EmbedClient) embedCacheFile {
	return embedCacheFile{Namespace: c.Namespace, Model: c.Model, Rows: map[string]embedRow{}}
}

// currentEntryHashes is the content hash of each embeddable entry in root's corpus right now.
func currentEntryHashes(root string) map[string]string {
	out := map[string]string{}
	for _, h := range corpusBlocks(root) {
		e := EntryFromHit(h)
		if e.Archived || e.Superseded {
			continue
		}
		if _, dup := out[e.ID]; !dup {
			out[e.ID] = textHash(entryEmbedText(e))
		}
	}
	return out
}

// EmbedEntries mints root's current (non-archived) entries into the profile's record, calling the endpoint only for
// new or changed content. While the record is valid it does nothing and returns ErrEmbedValid; embedded is 0 on a no-op.
func EmbedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, false)
}

// ReembedEntries is EmbedEntries' manual override: it re-embeds every row of root once the override window is open
// (the record's last embedOverrideAt, or expired/missing) and returns ErrReembedEarly before that.
func ReembedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	return embedEntries(ctx, root, c, true)
}

// checkEmbedLease is nil when a mint may start on rec: expired or missing, or force inside the override window.
func checkEmbedLease(rec embedCacheFile, now time.Time, force bool) error {
	if force {
		if at, open := reembedOpensAt(rec, now); !open {
			return fmt.Errorf("%w: opens at %s", ErrReembedEarly, at.Format("15:04"))
		}
		return nil
	}
	if embedValid(rec, now) {
		return fmt.Errorf("%w until %s", ErrEmbedValid, mintedTime(rec).Add(embedValidity).Format("15:04"))
	}
	return nil
}

func embedEntries(ctx context.Context, root string, c *EmbedClient, force bool) (embedded, total int, err error) {
	if !safeEmbedProfile(c.Profile) {
		return 0, 0, fmt.Errorf("embeddings: unsafe profile name %q", c.Profile)
	}
	now := time.Now()
	rec, err := readEmbedCache(c.Profile)
	if err != nil {
		return 0, 0, err
	}
	if err := checkEmbedLease(rec, now, force); err != nil {
		return 0, 0, err
	}
	if force || rec.Namespace != c.Namespace || rec.Rows == nil {
		rec = freshEmbedCache(c)
	}
	ids, texts, hashes, total := pendingEmbeds(root, rec)
	started := now.UnixNano()
	owner, err := newEmbedOwner()
	if err != nil {
		return 0, total, err
	}
	held, ok, err := claimEmbedMint(c.Profile, owner, now, embedClaimExpiry(ctx, now)) // always, even with nothing pending
	if err != nil {
		return 0, total, err
	}
	if !ok {
		return 0, total, fmt.Errorf("%w until %s", ErrEmbedBusy, held.Format("15:04"))
	}
	run := &embedRun{c: c, root: root, force: force, fresh: map[string]embedRow{}, started: started}
	var embedErr error
	for i := 0; i < len(texts) && embedErr == nil; i += embedBatch { // network calls stay outside any transaction
		j := min(i+embedBatch, len(texts))
		vecs, err := c.Embed(ctx, texts[i:j])
		if err == nil && len(vecs) != j-i {
			err = fmt.Errorf("embedding endpoint returned %d vectors for %d texts", len(vecs), j-i)
		}
		if err != nil {
			embedErr = err
			break
		}
		for k, v := range vecs {
			run.freshDim = len(v)
			run.fresh[ids[i+k]] = embedRow{Root: root, Hash: hashes[i+k], Vec: encodeVec(normalize(v))}
		}
		embedded += j - i
	}
	run.current = currentEntryHashes(root)
	stored, cerr := commitEmbedMint(c.Profile, owner, started, func(cur embedCacheFile) embedCacheFile {
		out := run.merge(cur)
		if embedErr == nil { // a partial run keeps its paid-for rows but does not start the lease
			out.MintedAt = time.Now().UnixNano()
		}
		return out
	})
	if cerr != nil {
		releaseEmbedMint(c.Profile, owner) // the tx rolled back, so the claim is still ours
	}
	if !stored && cerr == nil {
		embedded = 0
		cerr = ErrEmbedSuperseded
	}
	return embedded, total, errors.Join(embedErr, cerr)
}

// pendingEmbeds lists root's live entries whose vector in cache is missing, stale or undecodable, and the live count.
func pendingEmbeds(root string, cache embedCacheFile) (ids, texts, hashes []string, total int) {
	live := map[string]bool{}
	cacheDim := cacheDimOf(cache)
	for _, h := range corpusBlocks(root) {
		e := EntryFromHit(h)
		if e.Archived || e.Superseded || live[e.ID] { // the first of a duplicated id wins
			continue
		}
		text := entryEmbedText(e)
		hash := textHash(text)
		live[e.ID] = true
		if row, ok := cache.Rows[e.ID]; ok && row.Hash == hash {
			v, decoded := decodeVec(row.Vec)
			if decoded && (cacheDim == 0 || len(v) == cacheDim) {
				continue
			}
		}
		ids, texts, hashes = append(ids, e.ID), append(texts, text), append(hashes, hash)
	}
	return ids, texts, hashes, len(live)
}

// embedRun is one mint's paid-for rows, merged into the record that is current at commit time.
type embedRun struct {
	c        *EmbedClient
	root     string
	force    bool // override: fresh rows replace the old ones even when their hash matches
	fresh    map[string]embedRow
	freshDim int
	started  int64
	current  map[string]string // content hash of each embeddable entry in root right now
}

func (r *embedRun) merge(cur embedCacheFile) embedCacheFile {
	if cur.Namespace != r.c.Namespace || cur.Rows == nil {
		cur = freshEmbedCache(r.c)
	}
	fresh := r.fresh
	if r.freshDim > 0 && cur.Dim != r.freshDim {
		if cur.DimAt > r.started { // a newer run already moved the dimension
			fresh = nil
		} else { // the endpoint's dimension moved: drop vectors it cannot match
			for id, row := range cur.Rows {
				if v, ok := decodeVec(row.Vec); ok && len(v) > 0 && len(v) != r.freshDim {
					delete(cur.Rows, id)
				}
			}
			cur.Dim, cur.DimAt = r.freshDim, r.started
		}
	}
	for id, row := range fresh {
		if want, ok := r.current[id]; !ok || row.Hash != want {
			continue
		}
		if old, have := cur.Rows[id]; have && !r.force && old.Hash == row.Hash && sameDim(old, row) {
			continue
		}
		cur.Rows[id] = row
	}
	r.shed(cur)
	return cur
}

func sameDim(a, b embedRow) bool {
	av, aok := decodeVec(a.Vec)
	bv, bok := decodeVec(b.Vec)
	return aok && bok && len(av) == len(bv)
}

// shed drops this root's rows whose entry is gone once the root holds more than embedMaxRows.
func (r *embedRun) shed(cur embedCacheFile) {
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
	query []float32
	rows  map[string]embedRow
}

// readProfileCache is the per-profile read NewSemantic uses; a var so tests can fail one profile.
var readProfileCache = readEmbedCache

// NewSemantic embeds query within embedRecallWait and pairs it with the cached vectors; any failure is the
// caller's cue to rank by keyword alone. The session profile's cache wins, else the pinned profile's.
func NewSemantic(query string) (*Semantic, error) {
	if os.Getenv("CLAUDE_RECALL_SEMANTIC") == "0" {
		return nil, errors.New("disabled by CLAUDE_RECALL_SEMANTIC=0")
	}
	var profile string
	var cache embedCacheFile
	var readErr error
	for _, name := range []string{providers.SessionProfile(), providers.Pin(providers.PinProfile)} {
		if name == "" || name == profile || !safeEmbedProfile(name) {
			continue
		}
		c, err := readProfileCache(name)
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
			return nil, fmt.Errorf("embedding cache unreadable: %w", readErr)
		}
		return nil, errors.New("no embedding cache")
	}
	attempt := time.Now()
	backoff := filepath.Join(StateDir(), embedCachePre+profile+".backoff")
	if st, err := os.Stat(backoff); err == nil && time.Since(st.ModTime()) < embedBackoff {
		return nil, fmt.Errorf("endpoint timed out recently; semantic paused until %s", st.ModTime().Add(embedBackoff).Format("15:04"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedRecallWait)
	defer cancel()
	type result struct {
		vec []float32
		err error
	}
	done := make(chan result, 1)
	go func() { // the credential path can block outside ctx's reach; abandon it at the deadline
		c, err := OpenEmbedClient(ctx, profile, cache.Model)
		if err != nil {
			done <- result{err: err}
			return
		}
		if cache.Namespace == "" || c.Namespace != cache.Namespace { // checked first: no prompt text leaves on a mismatch
			done <- result{err: errors.New("embedding cache belongs to another workspace or model")}
			return
		}
		vecs, err := c.Embed(ctx, []string{query})
		if err == nil && len(vecs) != 1 {
			err = errors.New("embedding endpoint returned no query vector")
		}
		if err != nil {
			done <- result{err: err}
			return
		}
		if !cacheDimMatches(cache, len(vecs[0])) {
			done <- result{err: errors.New("embedding dimension differs from the cache")}
			return
		}
		done <- result{vec: normalize(vecs[0])}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		if st, err := os.Stat(backoff); err == nil && st.ModTime().Before(attempt) { // keep a marker a racing recall just wrote
			_ = os.Remove(backoff)
		}
		return &Semantic{query: r.vec, rows: cache.Rows}, nil
	case <-ctx.Done():
		_ = paths.AtomicWrite(backoff, nil, 0o600) // a slow endpoint costs one prompt the full wait, not every prompt
		return nil, fmt.Errorf("query embedding exceeded %s", embedRecallWait)
	}
}

// Cosine is e's similarity to the query; false when e has no vector for its current content.
func (s *Semantic) Cosine(e Entry) (float64, bool) {
	row, ok := s.rows[e.ID]
	if !ok || row.Hash != textHash(entryEmbedText(e)) {
		return 0, false
	}
	v, ok := decodeVec(row.Vec)
	if !ok || len(v) != len(s.query) {
		return 0, false
	}
	var dot float64
	for i := range v {
		dot += float64(v[i]) * float64(s.query[i])
	}
	return dot, true
}

// dominantDim returns the most frequent decoded vector dimension, with ties going to the larger dimension.
func dominantDim(rows map[string]embedRow) int {
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
func cacheDimOf(c embedCacheFile) int {
	if c.Dim > 0 {
		return c.Dim
	}
	return dominantDim(c.Rows)
}

// cacheDimMatches reports whether the cache's vector dimension is dim.
func cacheDimMatches(c embedCacheFile, dim int) bool {
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
	return base64.StdEncoding.EncodeToString(b)
}

func decodeVec(s string) ([]float32, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, true
}

// PickEmbeddingModel chooses a ready embeddings endpoint from a live catalog, preferring gte then bge.
func PickEmbeddingModel(infos []providers.ModelInfo) (string, bool) {
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

// openDatabricksEmbed is profile's embeddings client; credentials come only from providers.Authorize.
func openDatabricksEmbed(ctx context.Context, profile, model string) (*EmbedClient, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	sel, err := providers.Select(reg, profile) // "" = the pin, then the registry default
	if err != nil {
		return nil, err
	}
	p := sel.Provider
	if p.Preset != providers.PresetDatabricks || p.Host == "" {
		return nil, fmt.Errorf("profile %s is not a databricks workspace", p.Name)
	}
	if model == "" {
		infos, err := providers.ListModels(ctx, reg, p)
		if err != nil {
			return nil, err
		}
		var ok bool
		if model, ok = PickEmbeddingModel(infos); !ok {
			return nil, fmt.Errorf("profile %s serves no embeddings endpoint", p.Name)
		}
	}
	url := strings.TrimRight(p.Host, "/") + "/serving-endpoints/" + model + "/invocations"
	ns := p.Name + "|" + strings.TrimRight(p.Host, "/") + "|" + model
	return &EmbedClient{Profile: p.Name, Model: model, Namespace: ns, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if base, secret, ok := routerLink(ctx); ok { // the daemon holds the warm credential and upstream connections
			via := base + "/p/" + p.Name + "/" + providers.ProtocolOpenAI + "/" + model + "/invocations"
			vecs, err := PostEmbeddings(ctx, via, texts, func(h http.Header) error { h.Set(proxy.ClientHeader, secret); return nil })
			if err == nil || ctx.Err() != nil {
				return vecs, err
			}
		}
		return PostEmbeddings(ctx, url, texts, func(h http.Header) error { return providers.Authorize(ctx, p, h) })
	}}, nil
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

// Candidates are the top k current entries of root's corpus by cosine alone (at least floor), for prompts that
// share no keyword with the entry they mean; an in-memory scan, no network.
func (s *Semantic) Candidates(root string, k int, floor float64, skip func(SearchHit) bool) (hits []SearchHit, skipped bool) {
	type scored struct {
		hit SearchHit
		cos float64
	}
	var all []scored
	for _, h := range corpusBlocks(root) {
		e := EntryFromHit(h)
		if e.Archived || e.Superseded {
			continue
		}
		if cos, ok := s.Cosine(e); ok && cos >= floor {
			if skip != nil && skip(h) {
				skipped = true // a qualifying match already in context
				continue
			}
			h.ID = e.ID
			all = append(all, scored{h, cos})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].cos > all[j].cos })
	for _, a := range all[:min(k, len(all))] {
		hits = append(hits, a.hit)
	}
	return hits, skipped
}
