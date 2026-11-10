package memory

import (
	"path/filepath"
	"strings"
)

// routedRepos are the repo stores a query names explicitly (key or origin basename as a whole word); no IDF guessing.
func routedRepos(query string) []repoEntry {
	entries, err := RepoDirs()
	if err != nil {
		return nil
	}
	q := strings.ToLower(query)
	var out []repoEntry
	for _, e := range entries {
		if routeMatch(q, e.Key) || routeMatch(q, filepath.Base(e.Origin.Origin)) {
			out = append(out, e)
		}
	}
	return out
}

// routeMatch reports whether query names raw as a whole word, with or without its leading dot.
func routeMatch(query, raw string) bool {
	name := strings.ToLower(strings.TrimPrefix(raw, "."))
	if len(name) < 4 {
		return false
	}
	return routeWord(query, strings.ToLower(raw)) || routeWord(query, name)
}

// RepoByName is the store whose key equals name, else the store whose origin basename does, for `--repo <name>`.
func RepoByName(name string) (repoEntry, bool) {
	entries, _ := RepoDirs()
	for _, e := range entries {
		if strings.EqualFold(e.Key, name) {
			return e, true
		}
	}
	for _, e := range entries {
		if strings.EqualFold(filepath.Base(e.Origin.Origin), name) {
			return e, true
		}
	}
	return repoEntry{}, false
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

// RepoRoot resolves --repo like recall: a catalog store name maps to its origin checkout, else a path.
func RepoRoot(repo string) string {
	if e, ok := RepoByName(repo); ok && repo != "" && e.Origin.Origin != "" {
		return e.Origin.Origin
	}
	return Root(repo)
}
