package memory

import (
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// recallCandidate records each selection gate without changing the selection.
type recallCandidate struct {
	ID, File, Tier              string
	BM25                        float64
	Matched, Needed             int
	Cosine                      *float64
	Blended, AccessBoost, Final float64
	Outcome                     string
	key                         string
}

// recallTrace is populated only when explaining a recall.
type recallTrace struct {
	Terms                []string
	Embeddings, CacheAge string
	CacheRows            int
	Silent               bool
	Rows                 []recallCandidate
}

func (t *recallTrace) outcome(key, outcome string) {
	if t == nil {
		return
	}
	for i := range t.Rows {
		if t.Rows[i].key == key {
			t.Rows[i].Outcome = outcome
		}
	}
}

func (t *recallTrace) candidate(c recallPick, need int, sem *Semantic) {
	if t == nil {
		return
	}
	key := blockKey(c.Hit.Block)
	for _, row := range t.Rows {
		if row.key == key {
			return
		}
	}
	tier := "current"
	if c.Entry.Archived {
		tier = "archive"
	}
	r := recallCandidate{ID: c.Entry.ID, File: c.Entry.Bank + "/" + c.Entry.File, Tier: tier,
		BM25: c.Hit.Score, Matched: c.Hit.Matched, Needed: need, Blended: c.Score,
		Final: c.Score, Outcome: "over cap", key: key}
	if c.Entry.Superseded {
		r.Outcome = "superseded"
	}
	if sem != nil {
		if cos, ok := sem.CosineHit(c.Hit); ok {
			r.Cosine = &cos
		}
	}
	t.Rows = append(t.Rows, r)
}

func (t *recallTrace) scores(c recallPick, boost float64) {
	if t == nil {
		return
	}
	for i := range t.Rows {
		if t.Rows[i].key == blockKey(c.Hit.Block) {
			t.Rows[i].Blended = c.Score
			t.Rows[i].AccessBoost = boost
			t.Rows[i].Final = c.Score + boost
		}
	}
}

// recallCacheInfo is diagnostic I/O, never used by the hook's normal path.
func recallCacheInfo(t *recallTrace) {
	t.CacheAge = "missing"
	for _, name := range []string{providers.SessionProfile(), providers.AgentRecord("claude").Profile} {
		if name == "" || !safeProfile.MatchString(name) {
			continue
		}
		cache, err := readProfileCache(name)
		if err != nil {
			t.CacheAge = "unreadable"
			continue
		}
		if cache.Model == "" || len(cache.Rows) == 0 {
			continue
		}
		t.CacheRows = len(cache.Rows)
		if cache.MintedAt > 0 {
			t.CacheAge = fmt.Sprint(time.Since(mintedTime(cache)).Round(time.Second))
		} else {
			t.CacheAge = "unminted"
		}
		return
	}
}

func (t *recallTrace) selected(picks []recallPick) {
	if t == nil {
		return
	}
	context, _, _ := RecallContext(picks)
	for i, p := range picks {
		outcome := "over cap"
		line := "- " + p.Entry.Pointer()
		if i < recallBodies {
			line = fmt.Sprintf("- [%s/%s] %s", p.Entry.Bank, p.Entry.File, textutil.CapRunes(recallFlat(p.Entry), recallBodyCapRunes, "…"))
		}
		if strings.Contains(context, "\n"+line) {
			outcome = "pointer"
			if i < recallBodies {
				outcome = "body"
			}
		}
		t.outcome(blockKey(p.Hit.Block), outcome)
	}
}

func (t *recallTrace) candidates(ranked []SearchHit, need int, sem *Semantic) {
	for _, archived := range []bool{false, true} {
		_, top := recallTier(ranked, archived, need)
		tierSem := sem
		if archived {
			tierSem = nil
		}
		for _, h := range ranked {
			c := recallTracePick(h)
			if c.Entry.Archived != archived {
				continue
			}
			c.Score, _ = scoreRecall(c, top, false, tierSem)
			t.candidate(c, need, tierSem)
		}
	}
}

func recallTracePick(h SearchHit) recallPick {
	first, _, _ := strings.Cut(h.Block, "\n")
	entry := Entry{ID: h.ID, Bank: h.Bank, File: h.File, Archived: h.Bank == "archive",
		Superseded: strings.Contains(first, "~~") || strings.Contains(first, "SUPERSEDED")}
	return recallPick{Entry: entry, Hit: h}
}
