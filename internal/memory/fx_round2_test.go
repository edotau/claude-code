package memory

import (
	"context"
	"path/filepath"
	"testing"
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
		return &EmbedClient{Model: "m", Namespace: "ns", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = make([]float32, dim)
				out[i][0] = 1
			}
			return out, nil
		}}
	}
	run := func(c *EmbedClient) int {
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
