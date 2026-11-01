package memory

import (
	"github.com/edotau/claude-code/internal/memory/bank"
	"path/filepath"

	"github.com/edotau/claude-code/internal/memstore"
)

// routedFiles are the bank and topic files of every named repo, other than root's own store.
func routedFiles(root, query string) []bank.CorpusFile {
	own := memstore.Dir(root)
	var out []bank.CorpusFile
	for _, e := range memstore.RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, storeFiles(e)...)
		}
	}
	return out
}

// storeFiles labels a repo's typed bank files and native topics with its store key.
func storeFiles(e memstore.RepoEntry) []bank.CorpusFile {
	var out []bank.CorpusFile
	for _, name := range bank.BankFiles() {
		out = append(out, bank.CorpusFile{Bank: e.Key, Path: filepath.Join(e.Dir, name), Mode: bank.SegEntries})
	}
	for _, path := range bank.SearchTopicFiles(e.Dir) {
		out = append(out, bank.CorpusFile{Bank: e.Key, Path: path, Mode: bank.SegNative})
	}
	return out
}
