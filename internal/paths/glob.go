package paths

import (
	"path"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

var globPatterns sync.Map

// foldGlobCase matches globs case-insensitively per FoldsCase; a var so a test can pin either rule.
var foldGlobCase = FoldsCase

// GlobMatch globs a cleaned path: "*" and "?" stay inside a segment, "**" crosses directories.
func GlobMatch(pattern, p string) bool {
	key, flags := pattern, ""
	if foldGlobCase {
		key, flags = "\x00"+pattern, "(?i)" // NUL never occurs in a cleaned pattern, so keys cannot collide
	}
	cached, ok := globPatterns.Load(key)
	if !ok {
		re, _ := regexp.Compile(flags + "^" + globRegexp(pattern) + "$")
		cached, _ = globPatterns.LoadOrStore(key, re)
	}
	re := cached.(*regexp.Regexp)
	return re != nil && re.MatchString(p)
}

func CleanGlob(s string) string {
	s = strings.ReplaceAll(s, "**", "\x00")
	s = path.Clean(strings.ReplaceAll(s, "\\", "/"))
	return strings.ReplaceAll(s, "\x00", "**")
}

func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case c == '*' && strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default: // copy one whole rune: string(byte) would turn UTF-8 bytes into Latin-1 characters
			_, size := utf8.DecodeRuneInString(g[i:])
			b.WriteString(regexp.QuoteMeta(g[i : i+size]))
			i += size - 1
		}
	}
	return b.String()
}
