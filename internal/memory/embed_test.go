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

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
	"go.etcd.io/bbolt"
)

// fakeEmbedServer answers {"input":[...]} with one 3-dim vector per text and counts the texts it embedded.
func fakeEmbedServer(t *testing.T, embedded *int) *EmbedClient {
	t.Helper()
	t.Setenv("CLAUDE_PROFILE", "fake") // NewSemantic reads the session profile's cache
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
	return &EmbedClient{Profile: "fake", Model: "fake-gte", Namespace: "fake|h|fake-gte", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return PostEmbeddings(ctx, srv.URL, texts, func(http.Header) error { return nil })
	}}
}

// expireEmbed ends the validity lease of profile "fake" so the next EmbedEntries may mint again.
func expireEmbed(t *testing.T) {
	t.Helper()
	f := loadEmbedCache("fake")
	f.MintedAt = 0
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
}

// An unchanged entry is not re-embedded; an edited one is, alone; the cosine follows the vectors.
func TestEmbedEntriesReusesCacheByContentHash(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir() // a non-repo root collapses to the home bank
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bank, "# D\n\n## 2026-10-01 — gamma choice <!-- id:aaaa0001 -->\n\nbody one\n\n## 2026-10-02 — delta choice <!-- id:aaaa0002 -->\n\nbody two\n")
	var calls int
	c := fakeEmbedServer(t, &calls)

	n, total, err := EmbedEntries(context.Background(), root, c)
	if err != nil || n != 2 || total != 2 || calls != 2 {
		t.Fatalf("first index: embedded %d of %d, %d texts sent, err %v", n, total, calls, err)
	}
	expireEmbed(t)
	if n, _, err = EmbedEntries(context.Background(), root, c); err != nil || n != 0 || calls != 2 {
		t.Fatalf("unchanged entries re-embedded: n=%d texts sent=%d err=%v", n, calls, err)
	}
	write(t, bank, "# D\n\n## 2026-10-01 — gamma choice <!-- id:aaaa0001 -->\n\nbody one\n\n## 2026-10-02 — delta choice <!-- id:aaaa0002 -->\n\nbody two EDITED\n")
	expireEmbed(t)
	if n, _, err = EmbedEntries(context.Background(), root, c); err != nil || n != 1 || calls != 3 {
		t.Fatalf("edit must re-embed only that entry: n=%d texts sent=%d err=%v", n, calls, err)
	}

	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	OpenEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil }
	t.Cleanup(func() { OpenEmbedClient = openDatabricksEmbed })
	sem, err := NewSemantic("gamma gamma")
	if err != nil {
		t.Fatal(err)
	}
	var gamma, delta float64
	for _, h := range corpusBlocks(root) {
		e := EntryFromHit(h)
		cos, ok := sem.Cosine(e)
		if !ok {
			t.Fatalf("entry %s has no vector", e.ID)
		}
		if e.ID == "aaaa0001" {
			gamma = cos
		} else {
			delta = cos
		}
	}
	if gamma <= delta {
		t.Errorf("a gamma query must sit closer to the gamma entry: gamma=%.3f delta=%.3f", gamma, delta)
	}
}

func TestPickEmbeddingModelPrefersGteThenBge(t *testing.T) {
	infos := []providers.ModelInfo{
		{ID: "databricks-claude-opus-5", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-qwen3-embedding-0-6b", Task: "llm/v1/embeddings", Ready: "READY"},
		{ID: "databricks-bge-large-en", Task: "llm/v1/embeddings", Ready: "READY"},
		{ID: "databricks-gte-large-en", Task: "llm/v1/embeddings", Ready: "NOT_READY"},
	}
	if got, ok := PickEmbeddingModel(infos); !ok || got != "databricks-bge-large-en" {
		t.Errorf("not-ready gte must be skipped; got %q %v", got, ok)
	}
	infos[3].Ready = "READY"
	if got, _ := PickEmbeddingModel(infos); got != "databricks-gte-large-en" {
		t.Errorf("gte first; got %q", got)
	}
	if _, ok := PickEmbeddingModel(infos[:1]); ok {
		t.Error("a chat-only catalog has no embedding model")
	}
}

// A run that commits while another's vectors landed meanwhile keeps them (the late writer merges, not overwrites).
func TestEmbedEntriesConcurrentRunsMerge(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bank, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Profile: "fake", Model: inner.Model, Namespace: inner.Namespace, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // while run A is mid-embed, run B indexes a second entry and finishes
			nested = true
			write(t, bank, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — delta two <!-- id:aaaa0002 -->\n\nbody\n")
			cur := freshEmbedCache(inner)
			cur.Rows["aaaa0002"] = embedRow{Root: root, Hash: currentEntryHashes(root)["aaaa0002"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache("fake", cur); err != nil {
				t.Error(err)
			}
		}
		return inner.Embed(ctx, texts)
	}}
	if _, _, err := EmbedEntries(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	rows := loadEmbedCache("fake").Rows
	if _, a := rows["aaaa0001"]; !a {
		t.Error("run A's row missing")
	}
	if _, b := rows["aaaa0002"]; !b {
		t.Error("run B's row was overwritten by run A")
	}
}

// A cache from another workspace or dimension is never compared: the namespace re-embeds, the dimension refuses.
func TestEmbedCacheNamespaceAndDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	if _, _, err := EmbedEntries(context.Background(), root, c); err != nil || calls != 1 {
		t.Fatalf("seed: %v calls=%d", err, calls)
	}
	other := &EmbedClient{Profile: "fake", Model: c.Model, Namespace: "other|https://w2|fake-gte", Embed: c.Embed}
	expireEmbed(t)
	if n, _, err := EmbedEntries(context.Background(), root, other); err != nil || n != 1 {
		t.Errorf("same model name on another workspace must re-embed: n=%d err=%v", n, err)
	}
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return c, nil } // namespace fake, cache is "other"
	before := calls
	if _, err := NewSemantic("q"); err == nil {
		t.Error("a client from a different namespace must not score the cache")
	}
	if calls != before {
		t.Errorf("the prompt reached the endpoint (%d texts) despite the namespace mismatch", calls-before)
	}
	OpenEmbedClient = func(context.Context, string, string) (*EmbedClient, error) {
		return &EmbedClient{Profile: "fake", Model: other.Model, Namespace: other.Namespace, Embed: func(context.Context, []string) ([][]float32, error) {
			return [][]float32{{1, 2, 3, 4}}, nil
		}}, nil
	}
	if _, err := NewSemantic("q"); err == nil {
		t.Error("a 4-dim query against 3-dim cached vectors must be refused")
	}
}

// The profile argument picks the workspace, ahead of the pinned default.
func TestOpenEmbedClientUsesGivenProfile(t *testing.T) {
	isolatedConfig1(t)
	_, _, err := providers.UpdateOverlayDoc(true, func(top map[string]json.RawMessage, _ *vault.Tx) error {
		top["providers"] = json.RawMessage(`{"dev": {"provider":"databricks","preset":"databricks","host":"https://dev.example.com"},` +
			`"qa": {"provider":"databricks","preset":"databricks","host":"https://qa.example.com"}}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROFILE", "qa")
	c, err := openDatabricksEmbed(context.Background(), "qa", "databricks-gte-large-en")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Namespace, "qa|") {
		t.Errorf("namespace %q does not follow the session profile", c.Namespace)
	}
}

// Rows pruned past the cap belong to the indexing root only; another root's vectors survive.
func TestEmbedPruneNeverCrossesRoots(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"), "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	f := embedCacheFile{Namespace: c.Namespace, Model: c.Model, Rows: map[string]embedRow{"other-live": {Root: "/other/repo", Hash: "h", Vec: ""}}}
	for i := range embedMaxRows + 1 {
		f.Rows[fmt.Sprintf("stale-%d", i)] = embedRow{Root: root, Hash: "h"}
	}
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EmbedEntries(context.Background(), root, c); err != nil {
		t.Fatal(err)
	}
	rows := loadEmbedCache("fake").Rows
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
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	v1 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion one\n"
	v2 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion two\n"
	write(t, bank, v1)
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Profile: "fake", Model: inner.Model, Namespace: inner.Namespace, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // the entry changes mid-run and a newer run embeds the new content first
			nested = true
			write(t, bank, v2)
			cur := freshEmbedCache(inner)
			cur.Rows["aaaa0001"] = embedRow{Root: root, Hash: currentEntryHashes(root)["aaaa0001"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache("fake", cur); err != nil {
				t.Error(err)
			}
		}
		return inner.Embed(ctx, texts)
	}}
	if _, _, err := EmbedEntries(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	want := currentEntryHashes(root)["aaaa0001"]
	if got := loadEmbedCache("fake").Rows["aaaa0001"].Hash; got != want {
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
		*hits = append(*hits, r.URL.Path+"|"+r.Header.Get(proxy.ClientHeader))
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
	vecs, err := c.Embed(context.Background(), []string{"q"})
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
			vecs, err := c.Embed(context.Background(), []string{"q"})
			if err != nil || len(vecs) != 1 {
				t.Fatalf("Embed = %v, %v; want the direct fallback's vector", vecs, err)
			}
			if len(directHits) != 1 || !strings.HasPrefix(directHits[0], "/serving-endpoints/gte-x/invocations") {
				t.Errorf("direct hits = %v, want one on the serving endpoint", directHits)
			}
		})
	}
}

// The session profile's own cache wins; a miss falls back to the pinned profile's cache and opens its client there.
func TestNewSemanticPerProfileCacheFallsBackToPin(t *testing.T) {
	isolatedConfig1(t)
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	if err := providers.SetPins(map[string]string{providers.PinProfile: "dev"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROFILE", "qa")
	seed := func(profile string) {
		row := embedRow{Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}
		f := embedCacheFile{Namespace: profile + "|h|gte", Model: "gte", Rows: map[string]embedRow{"aaaa0001": row}}
		if err := saveEmbedCache(profile, f); err != nil {
			t.Fatal(err)
		}
	}
	var opened []string
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		opened = append(opened, profile)
		return &EmbedClient{Profile: profile, Model: model, Namespace: profile + "|h|gte", Embed: func(context.Context, []string) ([][]float32, error) {
			return [][]float32{{1, 0, 0}}, nil
		}}, nil
	}
	seed("dev")
	if _, err := NewSemantic("q"); err != nil || len(opened) != 1 || opened[0] != "dev" {
		t.Fatalf("qa cache miss: opened %v err %v, want the pinned dev profile", opened, err)
	}
	seed("qa")
	opened = nil
	if _, err := NewSemantic("q"); err != nil || len(opened) != 1 || opened[0] != "qa" {
		t.Fatalf("qa cache present: opened %v err %v, want the session's own qa profile", opened, err)
	}
	if saveEmbedCache("../escape", embedCacheFile{}) == nil {
		t.Error("an unsafe profile name must not be stored")
	}
}

// A timed-out query pauses the semantic leg: the next prompt skips the endpoint instead of waiting again.
func TestNewSemanticBacksOffAfterTimeout(t *testing.T) {
	isolatedConfig1(t)
	t.Cleanup(SetEmbedRecallWait(50 * time.Millisecond))
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	t.Setenv("CLAUDE_PROFILE", "fake")
	f := embedCacheFile{Namespace: "fake|h|gte", Model: "gte", Rows: map[string]embedRow{"aaaa0001": {Hash: "h", Vec: encodeVec([]float32{1, 0, 0})}}}
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32 // the abandoned embed goroutine still runs when the test reads it
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(_ context.Context, profile, model string) (*EmbedClient, error) {
		opens.Add(1)
		return &EmbedClient{Profile: profile, Model: model, Namespace: "fake|h|gte", Embed: func(ctx context.Context, _ []string) ([][]float32, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}
	if _, err := NewSemantic("q"); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("first query: err %v, want the timeout", err)
	}
	start := time.Now()
	if _, err := NewSemantic("q"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("second query: err %v, want the backoff", err)
	}
	if opens.Load() != 1 || time.Since(start) > embedRecallWait/2 {
		t.Errorf("backoff still reached the endpoint: opens=%d took %s", opens.Load(), time.Since(start))
	}
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

// saveEmbedCache stores f as profile's record unconditionally (one Put); the mint path uses commitEmbedMint.
func saveEmbedCache(profile string, f embedCacheFile) error {
	if !safeEmbedProfile(profile) {
		return fmt.Errorf("embeddings: unsafe profile name %q", profile)
	}
	return updateEmbedDB(func(tx *bbolt.Tx) error { return putEmbedRecord(tx, profile, f) })
}

// dimEmbedClient answers every text with a dim-wide unit vector, calling hook (if any) before each batch.
func dimEmbedClient(dim int, hook func()) *EmbedClient {
	return &EmbedClient{Profile: "fake", Model: "m", Namespace: "ns", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
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
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	a := "## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n"
	b := "\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n"
	c3 := "\n## 2026-10-03 — three <!-- id:aaaa0003 -->\n\nbody\n"
	run := func(c *EmbedClient) int {
		expireEmbed(t)
		n, _, err := EmbedEntries(context.Background(), root, c)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	write(t, bank, "# D\n\n"+a+b)
	run(dimEmbedClient(3, nil))
	write(t, bank, "# D\n\n"+a+b+c3)
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
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bank, "# D\n\n## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n")
	newer := dimEmbedClient(2, nil)
	old := dimEmbedClient(3, func() { // the old-dimension run is mid-embed when the migration finishes
		cur := freshEmbedCache(newer)
		cur.Dim, cur.DimAt = 2, time.Now().UnixNano()
		for id, h := range currentEntryHashes(root) {
			cur.Rows[id] = embedRow{Root: root, Hash: h, Vec: encodeVec([]float32{1, 0})}
		}
		if err := saveEmbedCache("fake", cur); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := EmbedEntries(context.Background(), root, old); err != nil {
		t.Fatal(err)
	}
	c := loadEmbedCache("fake")
	if cacheDimOf(c) != 2 {
		t.Fatalf("stale run reset the dimension to %d", cacheDimOf(c))
	}
	for id, r := range c.Rows {
		if v, _ := decodeVec(r.Vec); len(v) != 2 {
			t.Errorf("row %s has dim %d, want 2", id, len(v))
		}
	}
}

// The prune must use the corpus as it is under the lock: a vector a concurrent run just committed survives.
func TestPruneKeepsRowsOfEntriesAddedMidRun(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	e1 := "## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n"
	e2 := "\n## 2026-10-02 — gamma two <!-- id:aaaa0002 -->\n\nbody\n"
	write(t, bank, "# D\n\n"+e1)
	var calls int
	inner := fakeEmbedServer(t, &calls)
	f := embedCacheFile{Namespace: inner.Namespace, Model: inner.Model, Rows: map[string]embedRow{}}
	for i := range embedMaxRows + 1 {
		f.Rows[fmt.Sprintf("stale-%d", i)] = embedRow{Root: root, Hash: "h"}
	}
	if err := saveEmbedCache("fake", f); err != nil {
		t.Fatal(err)
	}
	nested := false
	outer := &EmbedClient{Profile: "fake", Model: inner.Model, Namespace: inner.Namespace, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // a concurrent run adds an entry and commits its vector before this run merges
			nested = true
			write(t, bank, "# D\n\n"+e1+e2)
			cur := loadEmbedCache("fake")
			cur.Rows["aaaa0002"] = embedRow{Root: root, Hash: currentEntryHashes(root)["aaaa0002"], Vec: encodeVec([]float32{1, 0, 0})}
			if err := saveEmbedCache("fake", cur); err != nil {
				t.Error(err)
			}
		}
		return inner.Embed(ctx, texts)
	}}
	if _, _, err := EmbedEntries(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadEmbedCache("fake").Rows["aaaa0002"]; !ok {
		t.Error("the prune deleted a vector a concurrent run committed")
	}
}

// The dimension check is deterministic (dominant dimension), and an obsolete-dimension row is re-embedded.
func TestMixedDimensionsAreDeterministicAndReindexed(t *testing.T) {
	rows := map[string]embedRow{}
	for _, id := range []string{"a", "b", "c"} {
		rows[id] = embedRow{Vec: encodeVec([]float32{1, 0, 0})}
	}
	rows["d"] = embedRow{Vec: encodeVec([]float32{1, 0, 0, 0, 0})}
	for range 100 {
		if !cacheDimMatches(embedCacheFile{Rows: rows}, 3) || cacheDimMatches(embedCacheFile{Rows: rows}, 5) {
			t.Fatal("dimension check must follow the dominant dimension on every call")
		}
	}

	isolatedConfig1(t)
	root := t.TempDir()
	write(t, filepath.Join(HomeBankDir(), "decisionLog.md"),
		"# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nb\n\n## 2026-10-02 — gamma two <!-- id:aaaa0002 -->\n\nb\n\n## 2026-10-03 — gamma three <!-- id:aaaa0003 -->\n\nb\n")
	var calls int
	c := fakeEmbedServer(t, &calls)
	if n, _, err := EmbedEntries(context.Background(), root, c); err != nil || n != 3 {
		t.Fatalf("first index: n=%d err=%v", n, err)
	}
	cache := loadEmbedCache("fake")
	r := cache.Rows["aaaa0002"]
	r.Vec = encodeVec([]float32{1, 0, 0, 0, 0})
	cache.Rows["aaaa0002"] = r
	cache.MintedAt = 0 // lease over
	if err := saveEmbedCache("fake", cache); err != nil {
		t.Fatal(err)
	}
	if n, _, err := EmbedEntries(context.Background(), root, c); err != nil || n != 1 {
		t.Errorf("an obsolete-dimension row must be re-embedded alone: n=%d err=%v", n, err)
	}
}
