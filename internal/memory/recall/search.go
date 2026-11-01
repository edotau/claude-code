package recall

import (
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// SearchBanks ranks every block of root's local, home and archive banks plus native topics; top k (0 = all).
func SearchBanks(root, query string, k int) []bank.SearchHit {
	return SearchBanksTerms(root, query, bank.ContentTerms(query), k)
}

// SearchBanksTerms ranks banks using the caller's pretokenized content terms.
func SearchBanksTerms(root, query string, terms []string, k int) []bank.SearchHit {
	ranked, _, _ := searchBanksTerms(root, query, terms, k)
	return ranked
}

func searchBanksTerms(root, query string, terms []string, k int) ([]bank.SearchHit, []bank.CorpusFile, []bank.SegView) {
	corpus := bank.CorpusFiles(root)
	files := append(corpus, bank.RoutedFiles(root, query)...)
	segs := loadSegments(files, true)
	return bank.RankFiles(terms, k, files, segs), files[:len(corpus)], segs[:len(corpus)]
}

// SearchAll ranks the home tier plus every repo store as one corpus (`memory search --all`).
func SearchAll(query string, k int) []bank.SearchHit {
	files := bank.CorpusFiles(paths.ConfigDir())
	if entries, err := memstore.RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != memstore.HarnessStoreKey() {
				files = append(files, bank.StoreFiles(e)...)
			}
		}
	}
	return bank.RankFiles(bank.ContentTerms(query), k, files, loadSegments(files, false))
}
