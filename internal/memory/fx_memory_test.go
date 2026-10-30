package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// The prune must use the corpus as it is under the lock: a vector a concurrent run just committed survives.
func TestFxPruneKeepsRowsOfEntriesAddedMidRun(t *testing.T) {
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
func TestFxMixedDimensionsAreDeterministicAndReindexed(t *testing.T) {
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

// Every hit is credited to the bank dir that owns it: repo native/archive, and --all's local corpus.
func TestFxLogHitsCreditsOwningStore(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	a := gitRepo(t, t.TempDir(), "proja")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(da, "archive", "old.md"), "# A\n\n## 2026-01-01 — zebra archived quota <!-- id:bbbb0001 -->\n\nzebra quota\n")
	write(t, filepath.Join(da, "notes.md"), "---\nname: n\n---\nzebra native quota <!-- id:bbbb0002 -->\n")
	harness := Dir(paths.ConfigDir())
	write(t, filepath.Join(harness, "decisionLog.md"), "# D\n\n## 2026-01-01 — zebra local quota <!-- id:bbbb0003 -->\n\nzebra quota\n")

	now := time.Now()
	LogHits(a, SearchBanks(a, "zebra quota", 0), AccessSearch, now)
	got := ReturnCounts([]string{da}, now)
	home := ReturnCounts([]string{HomeBankDir()}, now)
	for _, id := range []string{"bbbb0001", "bbbb0002"} {
		if got[id] == 0 || home[id] != 0 {
			t.Errorf("%s must be credited to the repo store, not home: repo=%d home=%d", id, got[id], home[id])
		}
	}

	LogHits(a, SearchAll("zebra quota", 0), AccessSearch, now)
	if c := ReturnCounts([]string{harness}, now); c["bbbb0003"] == 0 {
		t.Errorf("--all's local hit must credit the harness store: %v", c)
	}
	if c := ReturnCounts([]string{da}, now); c["bbbb0003"] != 0 {
		t.Errorf("--all's local hit was credited to the caller's repo: %v", c)
	}
}
