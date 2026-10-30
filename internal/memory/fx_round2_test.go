package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// An endpoint that moved to a new dimension converges: stale-dimension rows are re-embedded once, then nothing repeats.
func TestFxEmbedFollowsEndpointDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	a := "## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n"
	b := "\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n"
	c3 := "\n## 2026-10-03 — three <!-- id:aaaa0003 -->\n\nbody\n"
	dimClient := func(dim int) *EmbedClient {
		return &EmbedClient{Profile: "fake", Model: "m", Namespace: "ns", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = make([]float32, dim)
				out[i][0] = 1
			}
			return out, nil
		}}
	}
	run := func(c *EmbedClient) int {
		expireEmbed(t)
		n, _, err := EmbedEntries(context.Background(), root, c)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	write(t, bank, "# D\n\n"+a+b)
	run(dimClient(3))
	write(t, bank, "# D\n\n"+a+b+c3)
	run(dimClient(2)) // the endpoint now answers 2 dims; the majority rows are 3
	for range 3 {
		run(dimClient(2))
	}
	if n := run(dimClient(2)); n != 0 {
		t.Fatalf("rows still re-embedded every run: n=%d", n)
	}
}

// A run that started before a dimension migration cannot undo it when it finishes last.
func TestFxStaleRunCannotRevertDimension(t *testing.T) {
	isolatedConfig1(t)
	root := t.TempDir()
	bank := filepath.Join(HomeBankDir(), "decisionLog.md")
	write(t, bank, "# D\n\n## 2026-10-01 — one <!-- id:aaaa0001 -->\n\nbody\n\n## 2026-10-02 — two <!-- id:aaaa0002 -->\n\nbody\n")
	mk := func(dim int, hook func()) *EmbedClient {
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
	newer := mk(2, nil)
	old := mk(3, func() { // the old-dimension run is mid-embed when the migration finishes
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
