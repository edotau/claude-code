package memory

import (
	"path/filepath"
)

// RoutedFiles are the bank and topic files of every named repo, other than root's own store.
func RoutedFiles(root, query string) []CorpusFile {
	own := Dir(root)
	var out []CorpusFile
	for _, e := range RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, StoreFiles(e)...)
		}
	}
	return out
}

// StoreFiles labels a repo's typed bank files and native topics with its store key.
func StoreFiles(e RepoEntry) []CorpusFile {
	var out []CorpusFile
	for _, name := range BankFiles() {
		out = append(out, CorpusFile{Bank: e.Key, Path: filepath.Join(e.Dir, name), Mode: SegEntries})
	}
	for _, path := range searchTopicFiles(e.Dir) {
		out = append(out, CorpusFile{Bank: e.Key, Path: path, Mode: SegNative})
	}
	return out
}
