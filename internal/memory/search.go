package memory

import (
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// SearchBanks ranks every block of root's local, home and archive banks plus native topics; top k (0 = all).
func SearchBanks(root, query string, k int) []SearchHit {
	return SearchBanksTerms(root, query, contentTerms(query), k)
}

// SearchBanksTerms ranks banks using the caller's pretokenized content terms.
func SearchBanksTerms(root, query string, terms []string, k int) []SearchHit {
	files := append(corpusFiles(root), routedFiles(root, query)...)
	return rankFiles(terms, k, files, loadSegments(files, true))
}

// SearchAll ranks the home tier plus every repo store as one corpus (`memory search --all`).
func SearchAll(query string, k int) []SearchHit {
	files := corpusFiles(paths.ConfigDir())
	if entries, err := memstore.RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != memstore.HarnessStoreKey() {
				files = append(files, storeFiles(e)...)
			}
		}
	}
	return rankFiles(contentTerms(query), k, files, loadSegments(files, false))
}

