package quality

import (
	"regexp"
	"strings"
	"sync"
)

// skipQuoted returns the index of the quote c closing the literal opened at i (or past the end), honoring \-escapes.
func skipQuoted(src string, i int, c byte) int {
	for i++; i < len(src); i++ {
		if src[i] == '\\' {
			i++
			continue
		}
		if c == '`' && src[i] == '$' && i+1 < len(src) && src[i+1] == '{' {
			i = skipInterpolation(src, i+2)
			continue
		}
		if src[i] == c {
			break
		}
	}
	return i
}

// skipInterpolation skips balanced braces and nested literals inside a template's ${...}.
func skipInterpolation(src string, i int) int {
	depth := 1
	for ; i < len(src); i++ {
		switch c := src[i]; c {
		case '\\':
			i++
		case '\'', '"', '`':
			i = skipQuoted(src, i, c)
		case '/':
			if end := skipComment(src, i); end != i {
				i = end
			} else if regexAllowedAt([]byte(src), i) {
				i = maskRegex([]byte(src), i)
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return i
}

// skipComment returns the end of the comment opening at i, or i when src[i] does not open one.
func skipComment(src string, i int) int {
	if i+1 < len(src) && (src[i+1] == '/' || src[i+1] == '*') {
		return commentEnd(src, i)
	}
	return i
}

// commentEnd returns the index of the last byte of the // or /* comment starting at i.
func commentEnd(src string, i int) int {
	end := "\n"
	if src[i+1] == '*' {
		end = "*/"
	}
	j := strings.Index(src[i+2:], end)
	if j < 0 {
		return len(src) - 1
	}
	return i + 2 + j + len(end) - 1
}

// stripStrings blanks '…', "…" and `…` literal contents so prompt prose is never read as a pin or operator.
func stripStrings(src string) string {
	b := []byte(src)
	for i := 0; i < len(b); i++ {
		c := b[i]
		// A comment's apostrophe ("don't") must not open a string and blank the real pin after it.
		if c == '/' && i+1 < len(b) && (b[i+1] == '/' || b[i+1] == '*') {
			i = maskComment(b, i)
			continue
		}
		if c == '/' && regexAllowedAt(b, i) {
			i = maskRegex(b, i)
			continue
		}
		if c != '\'' && c != '"' && c != '`' {
			continue
		}
		last := skipQuoted(src, i, c)
		blankBody(b, i+1, min(last, len(b)))
		i = last
	}
	return string(b)
}

// regexKeywords are words after which a `/` starts a regex literal rather than dividing.
var regexKeywords = map[string]bool{"return": true, "typeof": true, "instanceof": true, "in": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true, "yield": true, "await": true}

// forOfHeadRe matches the text before a for-of binding: `for (` with no `;` or `)` since.
var forOfHeadRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bfor\s*\([^;()\n]*$`) })

// regexAllowedAt reports whether the `/` at i is in expression-start position (a regex, not division).
func regexAllowedAt(b []byte, i int) bool {
	j := i - 1
	for j >= 0 && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
		j--
	}
	if j < 0 {
		return true
	}
	if strings.IndexByte("(,=:[!&|?{;+-*%<>~^", b[j]) >= 0 {
		return !postfixIncDec(b, j)
	}
	k := j
	for k >= 0 && isIdentByte(b[k]) {
		k--
	}
	if string(b[k+1:j+1]) == "of" { // `of` is a keyword only as a for-of head; `of / 2` is a variable
		return forOfHeadRe().Match(b[:k+1])
	}
	return regexKeywords[string(b[k+1:j+1])]
}

// postfixIncDec reports whether the +/- at j closes a postfix ++/-- (so a following `/` divides).
func postfixIncDec(b []byte, j int) bool {
	return (b[j] == '+' || b[j] == '-') && j > 0 && b[j-1] == b[j]
}

// maskRegex blanks a regex body keeping its delimiters; returns the closing `/`, or i if none closes on the line.
func maskRegex(b []byte, i int) int {
	inClass := false
	for j := i + 1; j < len(b) && b[j] != '\n'; j++ {
		switch b[j] {
		case '\\':
			j++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				blankBody(b, i+1, j)
				return j
			}
		}
	}
	return i
}

// blankBody replaces b[from:to] with spaces, keeping newlines so line numbers survive.
func blankBody(b []byte, from, to int) {
	for k := from; k < to; k++ {
		if b[k] != '\n' {
			b[k] = ' '
		}
	}
}

func maskComment(b []byte, i int) int {
	last := commentEnd(string(b), i) // the comment's final byte, so the caller resumes after the terminator
	for k := i; k <= last && k < len(b); k++ {
		if b[k] != '\n' { // keep line structure so callers can still count lines
			b[k] = ' '
		}
	}
	return last
}
