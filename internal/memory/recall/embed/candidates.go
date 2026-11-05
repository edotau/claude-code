package embed

import (
	"encoding/binary"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/memory/bank"
)

func hitHash(h bank.SearchHit) string { return textHash(h.File + "\x00" + h.Block) }

func newSemantic(query []float32, rows map[string]vectorRow) *Semantic {
	s := &Semantic{query: query, rows: rows, blocks: make(map[string]string, len(rows))}
	for id, row := range rows {
		if row.BlockHash != "" {
			s.blocks[row.BlockHash] = id
		}
	}
	return s
}

// CosineHit reuses the minted block hash and ID without constructing an Entry.
func (s *Semantic) CosineHit(h bank.SearchHit) (float64, bool) {
	id, ok := s.blocks[hitHash(h)]
	if !ok || h.ID != "" && h.ID != id {
		return 0, false
	}
	return s.cosineRow(s.rows[id])
}

func (s *Semantic) cosineRow(row vectorRow) (float64, bool) {
	if len(row.Vec) != 4*len(s.query) {
		return 0, false
	}
	var dot float64
	for i, q := range s.query {
		v := math.Float32frombits(binary.LittleEndian.Uint32([]byte(row.Vec[4*i : 4*i+4])))
		dot += float64(v) * float64(q)
	}
	return dot, true
}

// Candidates scores the already loaded corpus segments without parsing bank files or constructing entries.
func (s *Semantic) Candidates(files []bank.CorpusFile, segs []bank.SegView, k int, floor float64, skip func(bank.SearchHit) bool) (hits []bank.SearchHit, skipped bool) {
	var all []scoredHit
	for i, seg := range segs {
		if seg == nil || files[i].Bank == "archive" {
			continue
		}
		for doc := range seg.Docs() {
			block, _ := seg.Doc(doc)
			h, cos, ok := s.hitFor(files[i], block, floor)
			if !ok {
				continue
			}
			if skip != nil && skip(h) {
				skipped = true
				continue
			}
			all = append(all, scoredHit{h, cos})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].cos > all[j].cos })
	for _, a := range all[:min(k, len(all))] {
		hits = append(hits, a.hit)
	}
	return hits, skipped
}

type scoredHit struct {
	hit bank.SearchHit
	cos float64
}

// hitFor builds the hit for block of file f and its cosine; false for a struck-out block or one with no embedding at floor.
func (s *Semantic) hitFor(f bank.CorpusFile, block string, floor float64) (bank.SearchHit, float64, bool) {
	first, _, _ := strings.Cut(block, "\n")
	if strings.Contains(first, "~~") || strings.Contains(first, "SUPERSEDED") {
		return bank.SearchHit{}, 0, false
	}
	h := bank.SearchHit{Bank: f.Bank, File: filepath.Base(f.Path), Block: block}
	h.ID = s.blocks[hitHash(h)]
	cos, ok := s.cosineRow(s.rows[h.ID])
	if h.ID == "" || !ok || cos < floor {
		return h, 0, false
	}
	h.Dir = filepath.Dir(f.Path)
	if h.Bank == "knowledge" {
		h.Dir = filepath.Dir(h.Dir)
	}
	return h, cos, true
}
