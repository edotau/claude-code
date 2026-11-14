package memory

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"go.etcd.io/bbolt"
)

// fakeEmbedServer answers {"input":[...]} with one 3-dim vector per text and counts the texts it embedded.
func fakeEmbedServer(t *testing.T, embedded *int) *EmbedClient {
	t.Helper()
	t.Setenv("CLAUDE_PROFILE", "fake") // embedQuery reads the session profile's cache
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*embedded += len(in.Input)
		type row struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []row
		for i, s := range in.Input {
			data = append(data, row{i, []float32{float32(strings.Count(s, "gamma")), float32(strings.Count(s, "delta")), 0.1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return &EmbedClient{Model: "fake-gte", EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		return postEmbeddings(ctx, srv.URL, texts, func(http.Header) error { return nil })
	}}
}

// expireEmbed ends the validity lease of profile "fake" so the next embedChanged may mint again.
func expireEmbed(t *testing.T) {
	t.Helper()
	f := loadEmbedCache()
	f.MintedAt = 0
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
}

// wantEmbedded fails unless the run embedded wantN entries without error and the endpoint has seen wantCalls texts.
// mustEntryHashes is currentEntryHashes for tests: it fails the test on a corpus read error.
func mustEntryHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	h, err := currentEntryHashes(root)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func wantEmbedded(t *testing.T, label string, n, calls int, err error, wantN, wantCalls int) {
	t.Helper()
	if err != nil || n != wantN || calls != wantCalls {
		t.Fatalf("%s: n=%d texts sent=%d err=%v", label, n, calls, err)
	}
}

// An unchanged entry is not re-embedded; an edited one is, alone; the cosine follows the vectors.
func TestEmbedEntriesReusesCacheByContentHash(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir() // a non-repo root collapses to the home bank
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bankFile, "# D\n\n## 2026-10-01 — gamma choice <!-- id:aaaa0001 -->\n\nbody one\n\n## 2026-10-02 — delta choice <!-- id:aaaa0002 -->\n\nbody two\n")
	var calls int
	c := fakeEmbedServer(t, &calls)

	n, total, err := embedChanged(context.Background(), root, c)
	if err != nil || n != 2 || total != 2 || calls != 2 {
		t.Fatalf("first index: embedded %d of %d, %d texts sent, err %v", n, total, calls, err)
	}
	expireEmbed(t)
	n, _, err = embedChanged(context.Background(), root, c)
	wantEmbedded(t, "unchanged entries re-embedded", n, calls, err, 0, 2)
	write(t, bankFile, "# D\n\n## 2026-10-01 — gamma choice <!-- id:aaaa0001 -->\n\nbody one\n\n## 2026-10-02 — delta choice <!-- id:aaaa0002 -->\n\nbody two EDITED\n")
	expireEmbed(t)
	n, _, err = embedChanged(context.Background(), root, c)
	wantEmbedded(t, "edit must re-embed only that entry", n, calls, err, 1, 3)
	assertSemanticCloser(t, c, root)
}

// assertSemanticCloser opens a Semantic over c and requires a gamma query to sit closer to the gamma entry.
func assertSemanticCloser(t *testing.T, c *EmbedClient, root string) {
	t.Helper()
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil }
	t.Cleanup(func() { openEmbedClient = openDatabricksEmbed })
	sem, err := embedQuery("gamma gamma")
	if err != nil {
		t.Fatal(err)
	}
	gamma, delta := entryCosines(t, sem, root)
	if gamma <= delta {
		t.Errorf("a gamma query must sit closer to the gamma entry: gamma=%.3f delta=%.3f", gamma, delta)
	}
}

// entryCosines returns the query's cosine to entry aaaa0001 (gamma) and to the other entry (delta).
func entryCosines(t *testing.T, sem *Semantic, root string) (gamma, delta float64) {
	t.Helper()
	hits, err := corpusBlocks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		e := entryFromHit(h)
		cos, ok := sem.cosineHit(h)
		if !ok {
			t.Fatalf("entry %s has no vector", e.ID)
		}
		if e.ID == "aaaa0001" {
			gamma = cos
		} else {
			delta = cos
		}
	}
	return gamma, delta
}

func TestPickEmbeddingModelPrefersGteThenBge(t *testing.T) {
	infos := []providers.ModelInfo{
		{ID: "databricks-claude-opus-5", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-qwen3-embedding-0-6b", Task: "llm/v1/embeddings", Ready: "READY"},
		{ID: "databricks-bge-large-en", Task: "llm/v1/embeddings", Ready: "READY"},
		{ID: "databricks-gte-large-en", Task: "llm/v1/embeddings", Ready: "NOT_READY"},
	}
	if got, ok := pickEmbeddingModel(infos); !ok || got != "databricks-bge-large-en" {
		t.Errorf("not-ready gte must be skipped; got %q %v", got, ok)
	}
	infos[3].Ready = "READY"
	if got, _ := pickEmbeddingModel(infos); got != "databricks-gte-large-en" {
		t.Errorf("gte first; got %q", got)
	}
	if _, ok := pickEmbeddingModel(infos[:1]); ok {
		t.Error("a chat-only catalog has no embedding model")
	}
}

// A run that commits while another's vectors landed meanwhile keeps them (the late writer merges, not overwrites).
func TestEmbedEntriesConcurrentRunsMerge(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bankFile, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Model: inner.Model, EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // while run A is mid-embed, run B indexes a second entry and finishes
			nested = true
			write(t, bankFile, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — delta two <!-- id:aaaa0002 -->\n\nbody\n")
			cur := freshEmbedCache(inner)
			cur.Rows["aaaa0002"] = vectorRow{Root: root, Hash: mustEntryHashes(t, root)["aaaa0002"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache(cur); err != nil {
				t.Error(err)
			}
		}
		return inner.EmbedFunc(ctx, texts)
	}}
	if _, _, err := embedChanged(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	rows := loadEmbedCache().Rows
	if _, a := rows["aaaa0001"]; !a {
		t.Error("run A's row missing")
	}
	if _, b := rows["aaaa0002"]; !b {
		t.Error("run B's row was overwritten by run A")
	}
}

// A cache of another model or dimension is never compared: the model re-embeds, the dimension refuses.
func TestEmbedCacheModelAndDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	if _, _, err := embedChanged(context.Background(), root, c); err != nil || calls != 1 {
		t.Fatalf("seed: %v calls=%d", err, calls)
	}
	other := &EmbedClient{Model: c.Model + "-2", EmbedFunc: c.EmbedFunc}
	if n, _, err := embedChanged(context.Background(), root, other); err != nil || n != 1 {
		t.Errorf("another model name must re-embed inside the lease: n=%d err=%v", n, err)
	}
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil } // model fake-gte, cache is the other model
	before := calls
	if _, err := embedQuery("q"); err == nil {
		t.Error("a client of a different model must not score the cache")
	}
	if calls != before {
		t.Errorf("the prompt reached the endpoint (%d texts) despite the model mismatch", calls-before)
	}
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) {
		return &EmbedClient{Model: other.Model, EmbedFunc: func(context.Context, []string) ([][]float32, error) {
			return [][]float32{{1, 2, 3, 4}}, nil
		}}, nil
	}
	if _, err := embedQuery("q"); err == nil {
		t.Error("a 4-dim query against 3-dim cached vectors must be refused")
	}
}

// Rows pruned past the cap belong to the indexing root only; another root's vectors survive.
func TestEmbedPruneNeverCrossesRoots(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	f := vectorRecord{Model: c.Model, Rows: map[string]vectorRow{"other-live": {Root: "/other/repo", Hash: "h", Vec: ""}}}
	for i := range embedMaxRows + 1 {
		f.Rows[fmt.Sprintf("stale-%d", i)] = vectorRow{Root: root, Hash: "h"}
	}
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := embedChanged(context.Background(), root, c); err != nil {
		t.Fatal(err)
	}
	rows := loadEmbedCache().Rows
	if _, ok := rows["other-live"]; !ok {
		t.Error("another root's vector was pruned")
	}
	if _, ok := rows["stale-0"]; ok {
		t.Error("this root's stale rows should have been shed")
	}
	if rows["aaaa0001"].Root != root {
		t.Errorf("new row not tagged with its root: %+v", rows["aaaa0001"])
	}
}

// An older run that merges last must not overwrite the vector of content a newer run already embedded.
func TestEmbedMergeKeepsVectorMatchingCurrentContent(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	v1 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion one\n"
	v2 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion two\n"
	write(t, bankFile, v1)
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Model: inner.Model, EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // the entry changes mid-run and a newer run embeds the new content first
			nested = true
			write(t, bankFile, v2)
			cur := freshEmbedCache(inner)
			cur.Rows["aaaa0001"] = vectorRow{Root: root, Hash: mustEntryHashes(t, root)["aaaa0001"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache(cur); err != nil {
				t.Error(err)
			}
		}
		return inner.EmbedFunc(ctx, texts)
	}}
	if _, _, err := embedChanged(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	want := mustEntryHashes(t, root)["aaaa0001"]
	if got := loadEmbedCache().Rows["aaaa0001"].Hash; got != want {
		t.Errorf("cache holds the older run's vector: hash %s, current content %s", got, want)
	}
}

// embedViaRouter sets up a stub databricks profile whose host is direct, with routerLink pointing at base.
func embedViaRouter(t *testing.T, direct, base string) *EmbedClient {
	t.Helper()
	isolatedConfig1(t)
	_, _, err := providers.UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		top["providers"] = json.RawMessage(`{"dev": {"provider":"databricks","preset":"databricks","host":"` + direct + `","auth":{"type":"key","command":"echo tok"}}}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROFILE", "dev")
	origTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = trustAll
	t.Cleanup(func() { http.DefaultClient.Transport = origTransport })
	orig := routerLink
	routerLink = func(context.Context) (string, string, bool) { return base, "s3cret", true }
	t.Cleanup(func() { routerLink = orig })
	c, err := openDatabricksEmbed(context.Background(), "dev", "gte-x")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// trustAll lets DefaultClient reach the httptest TLS servers (the provider host must be https).
var trustAll = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

func vecServer(t *testing.T, hits *[]string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits = append(*hits, r.URL.Path+"|"+r.Header.Get(gateway.ClientHeader))
		if status != 200 {
			http.Error(w, "boom", status)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// With the router up, the query embedding goes through its /p/<profile>/openai/<model>/invocations path with the secret.
func TestEmbedGoesThroughRouterWithSecret(t *testing.T) {
	var proxyHits, directHits []string
	px, direct := vecServer(t, &proxyHits, 200), vecServer(t, &directHits, 200)
	c := embedViaRouter(t, direct.URL, px.URL)
	vecs, err := c.EmbedFunc(context.Background(), []string{"q"})
	if err != nil || len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Fatalf("Embed = %v, %v", vecs, err)
	}
	if len(proxyHits) != 1 || proxyHits[0] != "/p/dev/openai/gte-x/invocations|s3cret" {
		t.Errorf("proxy hits = %v, want one on the profile path with the secret", proxyHits)
	}
	if len(directHits) != 0 {
		t.Errorf("direct endpoint hit %v, want none while the router answers", directHits)
	}
}

// A router that errors or is unreachable falls back to the direct workspace call.
func TestEmbedFallsBackToDirectWhenRouterFails(t *testing.T) {
	for name, status := range map[string]int{"500": 500, "unreachable": 0} {
		t.Run(name, func(t *testing.T) {
			var proxyHits, directHits []string
			px, direct := vecServer(t, &proxyHits, 500), vecServer(t, &directHits, 200)
			base := px.URL
			if status == 0 {
				px.Close()
			}
			c := embedViaRouter(t, direct.URL, base)
			vecs, err := c.EmbedFunc(context.Background(), []string{"q"})
			if err != nil || len(vecs) != 1 {
				t.Fatalf("Embed = %v, %v; want the direct fallback's vector", vecs, err)
			}
			if len(directHits) != 1 || !strings.HasPrefix(directHits[0], "/serving-endpoints/gte-x/invocations") {
				t.Errorf("direct hits = %v, want one on the serving endpoint", directHits)
			}
		})
	}
}

// One cache serves every workspace: A's vectors are reused under B, recall reads them, and a model change re-embeds.
func TestSharedEmbedCacheAcrossWorkspaces(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), lease3Bank)
	var calls int
	c := fakeEmbedServer(t, &calls)
	var opened []string
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, _ string) (*EmbedClient, error) {
		opened = append(opened, profile)
		return c, nil
	}
	t.Setenv("CLAUDE_PROFILE", "wsa")
	if n, _, err := embedChanged(context.Background(), root, c); err != nil || n != 3 {
		t.Fatalf("index under A: n=%d err=%v", n, err)
	}
	expireEmbed(t) // past the lease, so only the shared rows can spare the second workspace the work
	t.Setenv("CLAUDE_PROFILE", "wsb")
	if n, _, err := embedChanged(context.Background(), root, c); err != nil || n != 0 || calls != 3 {
		t.Fatalf("index under B: n=%d texts sent=%d err=%v, want A's vectors reused", n, calls, err)
	}
	sem, err := embedQuery("gamma")
	if err != nil || len(sem.rows) != 3 || len(opened) != 1 || opened[0] != "wsb" {
		t.Fatalf("recall under B: err=%v rows=%d opened=%v, want A's 3 rows through wsb", err, len(sem.rows), opened)
	}
	other := &EmbedClient{Model: c.Model + "-2", EmbedFunc: c.EmbedFunc}
	if n, _, err := embedChanged(context.Background(), root, other); err != nil || n != 3 || loadEmbedCache().Model != other.Model {
		t.Fatalf("model change: n=%d model=%q err=%v, want all rows re-embedded", n, loadEmbedCache().Model, err)
	}
}

// A timed-out query pauses the semantic dispatch: the next prompt skips the endpoint instead of waiting again.
func TestNewSemanticBacksOffAfterTimeout(t *testing.T) {
	isolatedConfig1(t)
	prevWait := embedRecallWait
	embedRecallWait = 50 * time.Millisecond
	t.Cleanup(func() { embedRecallWait = prevWait })
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	f := vectorRecord{Model: "gte", Rows: map[string]vectorRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32 // the abandoned embed goroutine still runs when the test reads it
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		opens.Add(1)
		return &EmbedClient{Model: model, EmbedFunc: func(ctx context.Context, _ []string) ([][]float32, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}
	if _, err := embedQuery("q"); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("first query: err %v, want the timeout", err)
	}
	start := time.Now()
	if _, err := embedQuery("q"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("second query: err %v, want the backoff", err)
	}
	if opens.Load() != 1 || time.Since(start) > embedRecallWait/2 {
		t.Errorf("backoff still reached the endpoint: opens=%d took %s", opens.Load(), time.Since(start))
	}
}

// loadEmbedCache is readEmbedCache with every failure folded into an empty record.
func loadEmbedCache() vectorRecord {
	f, err := readEmbedCache()
	if err != nil {
		return vectorRecord{}
	}
	return f
}

// saveEmbedCache stores f as the shared record unconditionally; the mint path merges through vectorStore.Put.
func saveEmbedCache(f vectorRecord) error {
	return updateEmbedDB(func(tx *bbolt.Tx) error { return putEmbedRecord(tx, f) })
}

// dimEmbedClient answers every text with a dim-wide unit vector, calling hook (if any) before each batch.
func dimEmbedClient(dim int, hook func()) *EmbedClient {
	return &EmbedClient{Model: "m", EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
		if hook != nil {
			hook()
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, dim)
			out[i][0] = 1
		}
		return out, nil
	}}
}

// An endpoint that moved to a new dimension converges: stale-dimension rows are re-embedded once, then nothing repeats.
func TestEmbedFollowsEndpointDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	a := "## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n"
	b := "\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n"
	c3 := "\n## 2026-10-03 — three <!-- id:aaaa0003 -->\n\nbody\n"
	run := func(c *EmbedClient) int {
		expireEmbed(t)
		n, _, err := embedChanged(context.Background(), root, c)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	write(t, bankFile, "# D\n\n"+a+b)
	run(dimEmbedClient(3, nil))
	write(t, bankFile, "# D\n\n"+a+b+c3)
	run(dimEmbedClient(2, nil)) // the endpoint now answers 2 dims; the majority rows are 3
	for range 3 {
		run(dimEmbedClient(2, nil))
	}
	if n := run(dimEmbedClient(2, nil)); n != 0 {
		t.Fatalf("rows still re-embedded every run: n=%d", n)
	}
}

// A run that started before a dimension migration cannot undo it when it finishes last.
func TestStaleEmbedRunCannotRevertDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bankFile, "# D\n\n## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n")
	newer := dimEmbedClient(2, nil)
	old := dimEmbedClient(3, func() { // the old-dimension run is mid-embed when the migration finishes
		cur := freshEmbedCache(newer)
		cur.Dim, cur.DimAt = 2, time.Now().UnixNano()
		for id, h := range mustEntryHashes(t, root) {
			cur.Rows[id] = vectorRow{Root: root, Hash: h, Vec: encodeVec([]float32{1, 0})}
		}
		if err := saveEmbedCache(cur); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := embedChanged(context.Background(), root, old); err != nil {
		t.Fatal(err)
	}
	c := loadEmbedCache()
	if cacheDimOf(c) != 2 {
		t.Fatalf("stale run reset the dimension to %d", cacheDimOf(c))
	}
	for id, r := range c.Rows {
		if dim, _ := vecDim(r.Vec); dim != 2 {
			t.Errorf("row %s has dim %d, want 2", id, dim)
		}
	}
}

// The prune must use the corpus as it is under the lock: a vector a concurrent run just committed survives.
func TestPruneKeepsRowsOfEntriesAddedMidRun(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bankFile := filepath.Join(HomeBankDir(), "decisionLog.md")
	e1 := "## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n"
	e2 := "\n## 2026-10-02 — gamma two <!-- id:aaaa0002 -->\n\nbody\n"
	write(t, bankFile, "# D\n\n"+e1)
	var calls int
	inner := fakeEmbedServer(t, &calls)
	f := vectorRecord{Model: inner.Model, Rows: map[string]vectorRow{}}
	for i := range embedMaxRows + 1 {
		f.Rows[fmt.Sprintf("stale-%d", i)] = vectorRow{Root: root, Hash: "h"}
	}
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	nested := false
	outer := &EmbedClient{Model: inner.Model, EmbedFunc: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // a concurrent run adds an entry and commits its vector before this run merges
			nested = true
			write(t, bankFile, "# D\n\n"+e1+e2)
			cur := loadEmbedCache()
			cur.Rows["aaaa0002"] = vectorRow{Root: root, Hash: mustEntryHashes(t, root)["aaaa0002"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache(cur); err != nil {
				t.Error(err)
			}
		}
		return inner.EmbedFunc(ctx, texts)
	}}
	if _, _, err := embedChanged(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadEmbedCache().Rows["aaaa0002"]; !ok {
		t.Error("the prune deleted a vector a concurrent run committed")
	}
}

// The dimension check is deterministic (dominant dimension), and an obsolete-dimension row is re-embedded.
func TestMixedDimensionsAreDeterministicAndReindexed(t *testing.T) {
	rows := map[string]vectorRow{}
	for _, id := range []string{"a", "b", "c"} {
		rows[id] = vectorRow{Vec: encodeVec([]float32{1, 0, 0})}
	}
	rows["d"] = vectorRow{Vec: encodeVec([]float32{1, 0, 0, 0, 0})}
	for range 100 {
		if !cacheDimMatches(vectorRecord{Rows: rows}, 3) || cacheDimMatches(vectorRecord{Rows: rows}, 5) {
			t.Fatal("dimension check must follow the dominant dimension on every call")
		}
	}

	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"),
		"# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nb\n\n## 2026-10-02 — gamma two <!-- id:aaaa0002 -->\n\nb\n\n## 2026-10-03 — gamma three <!-- id:aaaa0003 -->\n\nb\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	if n, _, err := embedChanged(context.Background(), root, c); err != nil || n != 3 {
		t.Fatalf("first index: n=%d err=%v", n, err)
	}
	cache := loadEmbedCache()
	r := cache.Rows["aaaa0002"]
	r.Vec = encodeVec([]float32{1, 0, 0, 0, 0})
	cache.Rows["aaaa0002"] = r
	cache.MintedAt = 0 // lease over
	if err := saveEmbedCache(cache); err != nil {
		t.Fatal(err)
	}
	if n, _, err := embedChanged(context.Background(), root, c); err != nil || n != 1 {
		t.Errorf("an obsolete-dimension row must be re-embedded alone: n=%d err=%v", n, err)
	}
}

// pauseProfile writes profile's fresh backoff marker.
func pauseProfile(t *testing.T, profile string) {
	t.Helper()
	if err := paths.AtomicWrite(embedBackoffPath(profile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Given the session profile is paused, When semanticCache runs, Then the claude record's profile embeds the query.
func TestSemanticCachePausedProfileFallsThrough(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_PROFILE", "pa")
	vaulttest.SetRecord(t, "claude", "profile", "pb")
	row := vectorRow{Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}
	if err := saveEmbedCache(vectorRecord{Model: "gte", Rows: map[string]vectorRow{"aaaa0001": row}}); err != nil {
		t.Fatal(err)
	}
	pauseProfile(t, "pa")
	name, cache, err := semanticCache(context.Background())
	if err != nil || name != "pb" || cache.Model != "gte" {
		t.Fatalf("got %q %q err %v, want pb through the shared cache", name, cache.Model, err)
	}
	pauseProfile(t, "pb")
	vaulttest.SetRecord(t, "claude", "profile", "pb") // every candidate paused: the pause error must beat "no embedding cache"
	if _, _, err := semanticCache(context.Background()); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Errorf("pa and pb paused: err %v, want the pause error", err)
	}
}

// Given a vault lookup that blocks past the deadline, When semanticCache runs, Then it returns the budget error in time.
func TestSemanticCacheVaultReadBoundedByDeadline(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_PROFILE", "pa")
	pauseProfile(t, "pa") // the walk reaches the blocked vault read only past a paused session profile
	release, exited := make(chan struct{}), make(chan struct{})
	prev := claudeRecord
	t.Cleanup(func() { claudeRecord = prev })
	claudeRecord = func() string { defer close(exited); <-release; return "pb" }
	t.Cleanup(func() { close(release); <-exited })
	prevWait := embedRecallWait
	embedRecallWait = 50 * time.Millisecond
	t.Cleanup(func() { embedRecallWait = prevWait })
	ctx, cancel := context.WithTimeout(context.Background(), embedRecallWait)
	defer cancel()
	start := time.Now()
	_, _, err := semanticCache(ctx)
	if el := time.Since(start); el > time.Second {
		t.Errorf("semanticCache took %s with the vault blocked, want about the 50ms deadline", el)
	}
	if err == nil || !strings.Contains(err.Error(), "query embedding exceeded") {
		t.Errorf("err %v, want the budget-overrun error", err)
	}
}

// Given a run snapshot naming the session profile, When embedProfiles walks, Then the snapshot's profile leads, not the ambient one.
func TestEmbedProfilesFollowTheRunSnapshot(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_PROFILE", "ambient")
	prev := claudeRecord
	t.Cleanup(func() { claudeRecord = prev })
	claudeRecord = func() string { return "" }
	first := func(ctx context.Context) string {
		for name := range embedProfiles(ctx) {
			return name
		}
		return ""
	}
	if got := first(proc.WithValues(context.Background(), proc.Values{"CLAUDE_PROFILE": "explicit"})); got != "explicit" {
		t.Errorf("snapshot profile: first candidate %q, want explicit", got)
	}
	if got := first(context.Background()); got != "ambient" {
		t.Errorf("no snapshot: first candidate %q, want the ambient profile", got)
	}
	if got := first(proc.WithValues(context.Background(), proc.Values{})); got == "ambient" {
		t.Error("empty snapshot: the ambient profile leaked into the walk")
	}
}
