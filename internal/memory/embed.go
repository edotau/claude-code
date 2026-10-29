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
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// embedCacheSchema is the embedding cache's schema version; a mismatch is a miss, never migrated.
const embedCacheSchema = 1

const (
	embedBatch      = 16
	embedTextRunes  = 2000
	embedMaxRows    = 4000
	embedBodyLimit  = 32 << 20
	embedCacheName  = "embeddings.json"
	embedRecallWait = 1200 * time.Millisecond // hard budget for the per-prompt embedding, credential included
)

var embedFormat = schemas.Register(schemas.Format{
	Name: "memory-embeddings", Owner: "internal/memory", Kind: schemas.Cache, Version: embedCacheSchema,
	Glob: EmbedCachePath,
	New:  func() any { return &embedCacheFile{} },
	Keep: "rebuildable cache; a version mismatch is a miss",
})

// EmbedCachePath is the entry-vector cache, scoped by CLAUDE_CONFIG_DIR like StateDir.
func EmbedCachePath() string { return filepath.Join(StateDir(), embedCacheName) }

// embedCacheFile maps an entry id to the vector of its content; Model pins which embedding endpoint wrote it.
type embedCacheFile struct {
	Schema int                 `json:"schema"`
	Model  string              `json:"model"`
	Rows   map[string]embedRow `json:"rows"`
}

type embedRow struct {
	Hash string `json:"hash"` // sha256 of the embedded text
	Vec  string `json:"vec"`  // base64 little-endian float32, L2-normalised
}

// EmbedClient embeds texts with one named model; Embed honours ctx.
type EmbedClient struct {
	Model string
	Embed func(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenEmbedClient builds the active profile's client for model ("" = discover one); a var so tests can fake it.
var OpenEmbedClient = openDatabricksEmbed

func loadEmbedCache() embedCacheFile {
	raw, err := os.ReadFile(EmbedCachePath())
	if err != nil {
		return embedCacheFile{}
	}
	var f embedCacheFile
	if !schemas.DecodeOK(embedFormat, raw, &f) {
		return embedCacheFile{}
	}
	return f
}

func saveEmbedCache(f embedCacheFile) error {
	f.Schema = embedCacheSchema
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(EmbedCachePath()), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(EmbedCachePath(), raw, 0o600)
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

// EmbedEntries embeds root's current (non-archived) entries, calling the endpoint only for new or changed content.
// Returns how many entries it embedded now and how many are indexed in all.
func EmbedEntries(ctx context.Context, root string, c *EmbedClient) (embedded, total int, err error) {
	cache := loadEmbedCache()
	if cache.Model != c.Model || cache.Rows == nil {
		cache = embedCacheFile{Model: c.Model, Rows: map[string]embedRow{}}
	}
	var ids, texts, hashes []string
	live := map[string]bool{}
	for _, h := range corpusBlocks(root) {
		e := EntryFromHit(h)
		if e.Archived || e.Superseded || live[e.ID] { // the first of a duplicated id wins
			continue
		}
		text := entryEmbedText(e)
		hash := textHash(text)
		live[e.ID] = true
		if row, ok := cache.Rows[e.ID]; ok && row.Hash == hash {
			continue
		}
		ids, texts, hashes = append(ids, e.ID), append(texts, text), append(hashes, hash)
	}
	for i := 0; i < len(texts); i += embedBatch {
		j := min(i+embedBatch, len(texts))
		vecs, err := c.Embed(ctx, texts[i:j])
		if err != nil {
			_ = saveEmbedCache(cache) // keep the batches already paid for
			return embedded, len(live), err
		}
		if len(vecs) != j-i {
			return embedded, len(live), fmt.Errorf("embedding endpoint returned %d vectors for %d texts", len(vecs), j-i)
		}
		for k, v := range vecs {
			cache.Rows[ids[i+k]] = embedRow{Hash: hashes[i+k], Vec: encodeVec(normalize(v))}
		}
		embedded += j - i
	}
	if len(cache.Rows) > embedMaxRows { // the cache is shared by every repo; shed rows this root no longer has
		for id := range cache.Rows {
			if !live[id] {
				delete(cache.Rows, id)
			}
		}
	}
	return embedded, len(live), saveEmbedCache(cache)
}

// Semantic scores entries against one embedded query, from the cached entry vectors.
type Semantic struct {
	query []float32
	rows  map[string]embedRow
}

// NewSemantic embeds query within embedRecallWait and pairs it with the cached vectors; any failure is the
// caller's cue to rank by keyword alone.
func NewSemantic(query string) (*Semantic, error) {
	if os.Getenv("CLAUDE_RECALL_SEMANTIC") == "0" {
		return nil, errors.New("disabled by CLAUDE_RECALL_SEMANTIC=0")
	}
	cache := loadEmbedCache()
	if cache.Model == "" || len(cache.Rows) == 0 {
		return nil, errors.New("no embedding cache")
	}
	ctx, cancel := context.WithTimeout(context.Background(), embedRecallWait)
	defer cancel()
	type result struct {
		vec []float32
		err error
	}
	done := make(chan result, 1)
	go func() { // the credential path can block outside ctx's reach; abandon it at the deadline
		c, err := OpenEmbedClient(ctx, cache.Model)
		if err != nil {
			done <- result{err: err}
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
		done <- result{vec: normalize(vecs[0])}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return &Semantic{query: r.vec, rows: cache.Rows}, nil
	case <-ctx.Done():
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

// openDatabricksEmbed is the active profile's embeddings client; credentials come only from providers.Authorize.
func openDatabricksEmbed(ctx context.Context, model string) (*EmbedClient, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	sel, err := providers.Select(reg, "")
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
	return &EmbedClient{Model: model, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
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
