package memory

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/memstore"
)

// routedFiles are the bank and topic files of every named repo, other than root's own store.
func routedFiles(root, query string) []corpusFile {
	own := memstore.Dir(root)
	var out []corpusFile
	for _, e := range memstore.RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, storeFiles(e)...)
		}
	}
	return out
}

// storeFiles labels a repo's typed bank files and native topics with its store key.
func storeFiles(e memstore.RepoEntry) []corpusFile {
	var out []corpusFile
	for _, name := range bankFiles {
		out = append(out, corpusFile{e.Key, filepath.Join(e.Dir, name), segEntries})
	}
	for _, path := range searchTopicFiles(e.Dir) {
		out = append(out, corpusFile{e.Key, path, segNative})
	}
	return out
}
