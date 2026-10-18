package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RoutedRepos are the repo stores a query names explicitly (key or origin basename as a whole word); no IDF guessing.
func RoutedRepos(query string) []RepoEntry {
	if !CutOver() {
		return nil
	}
	entries, err := RepoDirs()
	if err != nil {
		return nil
	}
	q := strings.ToLower(query)
	var out []RepoEntry
	for _, e := range entries {
		for _, name := range []string{e.Key, filepath.Base(e.Origin.Origin)} {
			name = strings.ToLower(strings.TrimPrefix(name, "."))
			if len(name) >= 4 && regexp.MustCompile(`(^|[^a-z0-9.-])`+regexp.QuoteMeta(name)+`($|[^a-z0-9-])`).MatchString(q) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// routedBlocks are the bank and topic blocks of every repo the query names, other than root's own store.
func routedBlocks(root, query string) []SearchHit {
	own := Dir(root)
	var out []SearchHit
	for _, e := range RoutedRepos(query) {
		if e.Dir != own {
			out = append(out, storeBlocks(e)...)
		}
	}
	return out
}

// storeBlocks are one repo store's typed-file and native-topic blocks, labeled with the store key.
func storeBlocks(e RepoEntry) []SearchHit {
	var out []SearchHit
	for _, name := range bankFiles {
		data, err := os.ReadFile(filepath.Join(e.Dir, name))
		if err != nil {
			continue
		}
		_, blocks := entryBlocks(name, string(data))
		for _, b := range blocks {
			out = append(out, SearchHit{Bank: e.Key, File: name, Block: b})
		}
	}
	for _, path := range topicPaths(e.Dir) {
		if data, err := os.ReadFile(path); err == nil {
			if n, desc, body, ok := topicFrontmatter(string(data)); ok {
				out = append(out, SearchHit{Bank: e.Key, File: filepath.Base(path), Block: n + " — " + desc + "\n" + body})
			}
		}
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
