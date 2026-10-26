package memory

import (
	"path/filepath"
	"strings"
)

// RoutedRepos are the repo stores a query names explicitly (key or origin basename as a whole word); no IDF guessing.
func RoutedRepos(query string) []RepoEntry {
	entries, err := RepoDirs()
	if err != nil {
		return nil
	}
	q := strings.ToLower(query)
	var out []RepoEntry
	for _, e := range entries {
		for _, name := range []string{e.Key, filepath.Base(e.Origin.Origin)} {
			name = strings.ToLower(strings.TrimPrefix(name, "."))
			if len(name) >= 4 && routeWord(q, name) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// routedFiles are the bank and topic files of every named repo, other than root's own store.
func routedFiles(root, query string) []corpusFile {
	own := Dir(root)
	var out []corpusFile
	for _, e := range RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, storeFiles(e)...)
		}
	}
	return out
}

// storeFiles labels a repo's typed bank files and native topics with its store key.
func storeFiles(e RepoEntry) []corpusFile {
	var out []corpusFile
	for _, name := range bankFiles {
		out = append(out, corpusFile{e.Key, filepath.Join(e.Dir, name), segEntries})
	}
	for _, path := range searchTopicFiles(e.Dir) {
		out = append(out, corpusFile{e.Key, path, segNative})
	}
	return out
}

// RepoByName is the store whose key or origin basename equals name, for `--repo <name>`.
func RepoByName(name string) (RepoEntry, bool) {
	entries, _ := RepoDirs()
	for _, e := range entries {
		if strings.EqualFold(e.Key, name) || strings.EqualFold(filepath.Base(e.Origin.Origin), name) {
			return e, true
		}
	}
	return RepoEntry{}, false
}

// routeWord uses the same separator set at both ends, so dotted names stay whole.
func routeWord(query, name string) bool {
	word := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' }
	for offset := 0; offset <= len(query)-len(name); {
		i := strings.Index(query[offset:], name)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(name)
		if (i == 0 || !word(query[i-1])) && (end == len(query) || !word(query[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}
