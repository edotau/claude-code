package memory

import (
	"os"
	"path/filepath"
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
			if len(name) >= 4 && routeWord(q, name) {
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
	topics, contents := topicContents(e.Dir)
	for _, path := range topics {
		if data := contents[path]; data != nil {
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
