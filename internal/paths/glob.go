package paths

import (
	"path"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

var globPatterns sync.Map // globKey -> *regexp.Regexp (nil when the pattern does not compile)

type globKey struct {
	pattern string
	fold    bool
}

// foldGlobCase matches globs case-insensitively per FoldsCase; a var so a test can pin either rule.
var foldGlobCase = FoldsCase

// GlobMatch globs a cleaned path: "*" and "?" stay inside a segment, "**" crosses directories.
func GlobMatch(pattern, p string) bool {
	key := globKey{pattern, foldGlobCase}
	cached, ok := globPatterns.Load(key)
	if !ok {
		flags := ""
		if key.fold {
			flags = "(?i)"
		}
		re, _ := regexp.Compile(flags + "^" + globRegexp(pattern) + "$")
		cached, _ = globPatterns.LoadOrStore(key, re)
	}
	re := cached.(*regexp.Regexp)
	return re != nil && re.MatchString(p)
}

// globEscape marks the next rune of a pattern as literal (EscapeGlob); a backslash is no escape (CleanGlob: "/").
const globEscape = '\x01'

// EscapeGlob makes s match itself literally inside a GlobMatch pattern.
func EscapeGlob(s string) string {
	e := string(globEscape)
	return strings.NewReplacer(e, e+e, "*", e+"*", "?", e+"?").Replace(s)
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
		case c == globEscape && i+1 < len(g): // the next rune is literal
			_, size := utf8.DecodeRuneInString(g[i+1:])
			b.WriteString(regexp.QuoteMeta(g[i+1 : i+1+size]))
			i += size
		default: // copy one whole rune: string(byte) would turn UTF-8 bytes into Latin-1 characters
			_, size := utf8.DecodeRuneInString(g[i:])
			b.WriteString(regexp.QuoteMeta(g[i : i+size]))
			i += size - 1
		}
	}
	return b.String()
}
