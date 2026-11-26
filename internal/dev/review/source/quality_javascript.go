package source

import "strings"

// jsCode blanks literals and comments without moving source offsets or newlines.
func jsCode(content string) string {
	code := []byte(content)
	for i := 0; i < len(content); i++ {
		end := jsLiteralEnd(content, i)
		if end == i {
			continue
		}
		for j := i; j < end; j++ {
			if code[j] != '\n' {
				code[j] = ' '
			}
		}
		i = end - 1
	}
	return string(code)
}

func jsLiteralEnd(s string, i int) int {
	if strings.HasPrefix(s[i:], "//") {
		if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
			return i + n
		}
		return len(s)
	}
	if strings.HasPrefix(s[i:], "/*") {
		if n := strings.Index(s[i+2:], "*/"); n >= 0 {
			return i + n + 4
		}
		return len(s)
	}
	if s[i] == '/' && jsRegexStart(s[:i]) {
		return jsRegexEnd(s, i)
	}
	if s[i] == '`' {
		return jsTemplateEnd(s, i)
	}
	if s[i] == '\'' || s[i] == '"' {
		return jsStringEnd(s, i)
	}
	return i
}

func jsStringEnd(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case s[i]:
			return j + 1
		}
	}
	return len(s)
}

// jsFunctionBody ends a block at its matching brace and an expression at its statement boundary.
func jsFunctionBody(code string, start int) string {
	body := strings.TrimLeft(code[start:], " \t\r\n")
	if strings.HasPrefix(body, ":") {
		if n := strings.IndexByte(body, '{'); n >= 0 {
			body = body[n:]
		}
	}
	if !strings.HasPrefix(body, "{") {
		return code[start : start+jsExpressionEnd(code[start:])]
	}
	open := len(code) - len(body)
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return code[start : i+1]
			}
		}
	}
	return code[start:]
}

func jsExpressionEnd(code string) int {
	depth := 0
	for i := range len(code) {
		switch code[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return i
			}
			depth--
		case ';', ',', '\n':
			if depth == 0 {
				return i
			}
		}
	}
	return len(code)
}

func jsRegexStart(before string) bool {
	before = strings.TrimRight(before, " \t\r\n")
	if before == "" {
		return true
	}
	if strings.ContainsRune("(=:[,!&|?{;", rune(before[len(before)-1])) {
		return true
	}
	return strings.HasSuffix(before, "return") || strings.HasSuffix(before, "=>")
}

func jsRegexEnd(s string, start int) int {
	inClass := false
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return i + 1
			}
		case '\n':
			return start
		}
	}
	return start
}

func jsTemplateEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '`':
			return i + 1
		case '$':
			if strings.HasPrefix(s[i:], "${") {
				i = jsInterpolationEnd(s, i+2) - 1
			}
		}
	}
	return len(s)
}

func jsInterpolationEnd(s string, start int) int {
	depth := 1
	for i := start; i < len(s); i++ {
		if end := jsLiteralEnd(s, i); end > i {
			i = end - 1
			continue
		}
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}
