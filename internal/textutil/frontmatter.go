package textutil

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// fmRes lazily compiles the frontmatter regexes.
var fmRes = sync.OnceValue(func() (r struct {
	list, kv *regexp.Regexp
}) {
	r.list = regexp.MustCompile(`^\s*-\s+(.+)$`)
	r.kv = regexp.MustCompile(`^(\w[\w-]*):\s*(.*)$`)
	return
})

// ParseFrontmatter parses a YAML subset (string, bool, int, []string) from markdown; returns (frontmatter, body).
func ParseFrontmatter(content string) (map[string]any, string) {
	// BOM/CRLF-normalize first: StripFrontMatter anchors on \n, so a CRLF file would silently drop the frontmatter.
	content = strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	// A closing --- that ends the file has no trailing newline; add one so the delimiter matches.
	prefix, rest := stripFrontMatter([]byte(strings.TrimSuffix(content, "\n") + "\n"))
	if prefix == nil {
		return map[string]any{}, content
	}
	yaml := strings.TrimSuffix(strings.TrimPrefix(string(prefix), "---\n"), "---\n")
	body := strings.TrimSpace(string(rest))

	p := fmParser{fm: map[string]any{}}
	for line := range strings.SplitSeq(yaml, "\n") {
		// Blank and full-line comment lines carry no data and must not reset an open block list.
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			p.feed(line)
		}
	}
	return p.fm, body
}

// fmParser is the line-by-line frontmatter state: the open key and, for a block list, its items so far.
type fmParser struct {
	fm       map[string]any
	key      string
	list     []string
	haveList bool
}

// feed consumes one non-blank frontmatter line.
func (p *fmParser) feed(line string) {
	// YAML allows block-list items at the key's own indent, so an open list also takes "- item".
	if p.key != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || (p.haveList && strings.HasPrefix(line, "-"))) {
		p.continuation(line)
		return
	}
	kv := fmRes().kv.FindStringSubmatch(line)
	if kv == nil {
		p.key, p.list, p.haveList = "", nil, false
		return
	}
	key, value := kv[1], strings.TrimSpace(kv[2])
	p.key, p.list, p.haveList = key, nil, false
	p.fm[key] = parseScalar(value)
	if value == "" {
		p.haveList = true
	}
}

// continuation handles an indented line: a list item under a block list, else a folded string continuation.
func (p *fmParser) continuation(line string) {
	if p.haveList && (len(p.list) > 0 || fmRes().list.MatchString(line)) {
		if lm := fmRes().list.FindStringSubmatch(line); lm != nil {
			p.list = append(p.list, stripQuotes(strings.TrimSpace(lm[1])))
			p.fm[p.key] = p.list
		}
		return
	}
	if existing, isString := p.fm[p.key].(string); isString {
		p.fm[p.key] = strings.TrimSpace(existing + " " + strings.TrimSpace(line))
	}
}

// stripFrontMatter splits a leading "---\n" ... "\n---\n" block (delimiters included) from source; prefix is nil if absent.
func stripFrontMatter(source []byte) (prefix, content []byte) {
	delim := []byte("---\n")
	if !bytes.HasPrefix(source, delim) {
		return nil, source
	}
	rest := source[len(delim):]
	if bytes.HasPrefix(rest, delim) {
		return source[:2*len(delim)], source[2*len(delim):]
	}
	idx := bytes.Index(rest, []byte("\n---\n"))
	if idx < 0 {
		return nil, source
	}
	end := len(delim) + idx + len("\n---\n")
	return source[:end], source[end:]
}

// parseScalar converts one frontmatter value: block-scalar marker → "", inline list, bool, int, else string.
func parseScalar(value string) any {
	switch value {
	case "", "|", "|-", ">", ">-":
		return ""
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		items := []string{}
		for _, part := range splitFlowItems(value[1 : len(value)-1]) {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, stripQuotes(part))
			}
		}
		return items
	}
	switch strings.ToLower(value) {
	case "true", "yes":
		return true
	case "false", "no":
		return false
	}
	if IsDigits(value) {
		if n, err := strconv.Atoi(value); err == nil {
			return n
		}
		return value
	}
	return stripQuotes(value)
}

// splitFlowItems splits an inline list on commas, ignoring commas inside a quoted item.
func splitFlowItems(s string) []string {
	var out []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case (c == '"' || c == '\'') && strings.TrimSpace(s[start:i]) == "":
			quote = c // a quote opens an item only at its start, so "don't" stays plain
		case c == ',':
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// stripQuotes removes one matching pair of surrounding quotes, leaving inner and unpaired quotes intact.
func stripQuotes(s string) string {
	if n := len(s); n >= 2 && (s[0] == '"' || s[0] == '\'') && s[n-1] == s[0] {
		return s[1 : n-1]
	}
	return s
}
