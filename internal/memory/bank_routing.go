package memory

import (
	"path/filepath"
)

// routedFiles are the bank and topic files of every named repo, other than root's own store.
func routedFiles(root, query string) []corpusFile {
	own := Dir(root)
	var out []corpusFile
	for _, e := range routedRepos(query) {
		if e.Dir != own {
			out = append(out, storeFiles(e)...)
		}
	}
	return out
}

// storeFiles labels a repo's typed bank files and native topics with its store key.
func storeFiles(e repoEntry) []corpusFile {
	var out []corpusFile
	for _, name := range BankFiles() {
		out = append(out, corpusFile{Bank: e.Key, Path: filepath.Join(e.Dir, name), Mode: SegEntries})
	}
	for _, path := range searchTopicFiles(e.Dir) {
		out = append(out, corpusFile{Bank: e.Key, Path: path, Mode: SegNative})
	}
	return out
}
