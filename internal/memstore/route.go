package memstore

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
	for offset := 0; offset <= len(query)-len(name); {
		i := strings.Index(query[offset:], name)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(name)
		if (i == 0 || !routeWordByte(query[i-1])) && (end == len(query) || !routeWordByte(query[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

// routeWordByte reports whether c continues a route word.
func routeWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-'
}
