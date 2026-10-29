package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// fakeEmbedServer answers {"input":[...]} with one 3-dim vector per text and counts the texts it embedded.
func fakeEmbedServer(t *testing.T, embedded *int) *EmbedClient {
	t.Helper()
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
	return &EmbedClient{Model: "fake-gte", Namespace: "fake|h|fake-gte", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return PostEmbeddings(ctx, srv.URL, texts, func(http.Header) error { return nil })
	}}
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
	if n, _, err = EmbedEntries(context.Background(), root, c); err != nil || n != 0 || calls != 2 {
		t.Fatalf("unchanged entries re-embedded: n=%d texts sent=%d err=%v", n, calls, err)
	}
	write(t, bank, "# D\n\n## 2026-10-01 — gamma choice <!-- id:aaaa0001 -->\n\nbody one\n\n## 2026-10-02 — delta choice <!-- id:aaaa0002 -->\n\nbody two EDITED\n")
	if n, _, err = EmbedEntries(context.Background(), root, c); err != nil || n != 1 || calls != 3 {
		t.Fatalf("edit must re-embed only that entry: n=%d texts sent=%d err=%v", n, calls, err)
	}

	t.Setenv("CLAUDE_RECALL_SEMANTIC", "")
	OpenEmbedClient = func(context.Context, string) (*EmbedClient, error) { return c, nil }
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

// Two index runs overlapping in time keep each other's vectors (the late writer merges, not overwrites).
func TestEmbedEntriesConcurrentRunsMerge(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bank, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n")
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Model: inner.Model, Namespace: inner.Namespace, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // while run A is mid-embed, run B indexes a second entry and finishes
			nested = true
			write(t, bank, "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — delta two <!-- id:aaaa0002 -->\n\nbody\n")
			if _, _, err := EmbedEntries(ctx, root, inner); err != nil {
				t.Error(err)
			}
		}
		return inner.Embed(ctx, texts)
	}}
	if _, _, err := EmbedEntries(context.Background(), root, outer); err != nil {
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
	other := &EmbedClient{Model: c.Model, Namespace: "other|https://w2|fake-gte", Embed: c.Embed}
	if n, _, err := EmbedEntries(context.Background(), root, other); err != nil || n != 1 {
		t.Errorf("same model name on another workspace must re-embed: n=%d err=%v", n, err)
	}
	prev := OpenEmbedClient
	t.Cleanup(func() { OpenEmbedClient = prev })
	OpenEmbedClient = func(context.Context, string) (*EmbedClient, error) { return c, nil } // namespace fake, cache is "other"
	before := calls
	if _, err := NewSemantic("q"); err == nil {
		t.Error("a client from a different namespace must not score the cache")
	}
	if calls != before {
		t.Errorf("the prompt reached the endpoint (%d texts) despite the namespace mismatch", calls-before)
	}
	OpenEmbedClient = func(context.Context, string) (*EmbedClient, error) {
		return &EmbedClient{Model: other.Model, Namespace: other.Namespace, Embed: func(context.Context, []string) ([][]float32, error) {
			return [][]float32{{1, 2, 3, 4}}, nil
		}}, nil
	}
	if _, err := NewSemantic("q"); err == nil {
		t.Error("a 4-dim query against 3-dim cached vectors must be refused")
	}
}

// The launched session's profile (CLAUDE_PROFILE) picks the workspace, ahead of the pinned default.
func TestOpenEmbedClientFollowsSessionProfile(t *testing.T) {
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
	c, err := openDatabricksEmbed(context.Background(), "databricks-gte-large-en")
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
	if err := saveEmbedCache(f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EmbedEntries(context.Background(), root, c); err != nil {
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
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	v1 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion one\n"
	v2 := "# D\n\n## 2026-10-01 — gamma one <!-- id:aaaa0001 -->\n\nversion two\n"
	write(t, bank, v1)
	var calls int
	inner := fakeEmbedServer(t, &calls)
	nested := false
	outer := &EmbedClient{Model: inner.Model, Namespace: inner.Namespace, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		if !nested { // the entry changes mid-run and a newer run embeds the new content first
			nested = true
			write(t, bank, v2)
			if _, _, err := EmbedEntries(ctx, root, inner); err != nil {
				t.Error(err)
			}
		}
		return inner.Embed(ctx, texts)
	}}
	if _, _, err := EmbedEntries(context.Background(), root, outer); err != nil {
		t.Fatal(err)
	}
	want := currentEntryHashes(root)["aaaa0001"]
	if got := loadEmbedCache().Rows["aaaa0001"].Hash; got != want {
		t.Errorf("cache holds the older run's vector: hash %s, current content %s", got, want)
	}
}
