package workflows

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
			} else if regexAllowedAt(src, i) {
				i = regexClose(src, i)
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

// stripStrings blanks literal text, comments and regex bodies so prompt prose is never read as a pin or operator.
func stripStrings(src string) string {
	b := []byte(src)
	maskCode(src, b, 0, false)
	return string(b)
}

// maskCode masks src's code from i into b; inInterp returns at the `}` closing a template's ${, else at the end.
func maskCode(src string, b []byte, i int, inInterp bool) int {
	depth := 0
	for ; i < len(src); i++ {
		switch c := src[i]; c {
		case '/': // a comment's apostrophe ("don't") must not open a string and blank the real pin after it
			if end := skipComment(src, i); end != i {
				blankBody(b, i, end+1)
				i = end
			} else if regexAllowedAt(b, i) {
				i = maskRegex(b, i)
			}
		case '\'', '"':
			last := skipQuoted(src, i, c)
			blankBody(b, i+1, min(last, len(b)))
			i = last
		case '`':
			i = maskTemplate(src, b, i)
		case '{':
			depth++
		case '}':
			if inInterp && depth == 0 {
				return i
			}
			depth--
		}
	}
	return len(src)
}

// maskTemplate blanks a template literal's text but masks each ${...} as code; returns the closing backtick.
func maskTemplate(src string, b []byte, i int) int {
	for i++; i < len(src); i++ {
		switch {
		case src[i] == '\\':
			blankBody(b, i, min(i+2, len(b)))
			i++
		case src[i] == '`':
			return i
		case src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			i = maskCode(src, b, i+2, true)
		default:
			blankBody(b, i, i+1)
		}
	}
	return len(src)
}

// regexKeywords are words after which a `/` starts a regex literal rather than dividing.
var regexKeywords = map[string]bool{"return": true, "typeof": true, "instanceof": true, "in": true,
	"new": true, "delete": true, "void": true, "throw": true, "case": true, "do": true, "else": true, "yield": true, "await": true}

// forOfHeadRe matches the text before a for-of binding: `for (` with no `;` or `)` since.
var forOfHeadRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bfor\s*\([^;()\n]*$`) })

// regexAllowedAt reports whether the `/` at i is in expression-start position (a regex, not division).
func regexAllowedAt[T string | []byte](b T, i int) bool {
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
		return forOfHeadRe().MatchString(string(b[:k+1]))
	}
	return regexKeywords[string(b[k+1:j+1])]
}

// postfixIncDec reports whether the +/- at j closes a postfix ++/-- (so a following `/` divides).
func postfixIncDec[T string | []byte](b T, j int) bool {
	return (b[j] == '+' || b[j] == '-') && j > 0 && b[j-1] == b[j]
}

// maskRegex blanks a regex body keeping its delimiters; returns the closing `/`, or i if none closes on the line.
func maskRegex(b []byte, i int) int {
	end := regexClose(b, i)
	blankBody(b, i+1, max(end, i+1))
	return end
}

// regexClose returns the `/` closing the regex opened at i, or i if none closes on the line.
func regexClose[T string | []byte](b T, i int) int {
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
