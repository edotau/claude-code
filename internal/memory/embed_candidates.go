package memory

import "sort"

// Candidates returns the top k corpus entries by cosine alone, without network calls.
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
