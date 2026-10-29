package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
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
	return &EmbedClient{Model: "fake-gte", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
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
