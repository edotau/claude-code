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
	r.list = regexp.MustCompile(`^\s+-\s+(.+)$`)
	r.kv = regexp.MustCompile(`^(\w[\w-]*):\s*(.*)$`)
	return
})

// ParseFrontmatter parses a YAML subset (string, bool, int, []string) from markdown; returns (frontmatter, body).
func ParseFrontmatter(content string) (map[string]any, string) {
	// BOM/CRLF-normalize first: StripFrontMatter anchors on \n, so a CRLF file would silently drop the frontmatter.
	content = strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	prefix, rest := stripFrontMatter([]byte(content))
	if prefix == nil {
		return map[string]any{}, content
	}
	yaml := strings.TrimSuffix(strings.TrimPrefix(string(prefix), "---\n"), "---\n")
	body := strings.TrimSpace(string(rest))

	fm := map[string]any{}
	currentKey := ""
	var currentList []string
	haveList := false

	for line := range strings.SplitSeq(yaml, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if currentKey != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			if haveList {
				if lm := fmRes().list.FindStringSubmatch(line); lm != nil {
					currentList = append(currentList, stripQuotes(strings.TrimSpace(lm[1])))
					fm[currentKey] = currentList
				}
				continue
			}
			existing, _ := fm[currentKey].(string)
			fm[currentKey] = strings.TrimSpace(existing + " " + strings.TrimSpace(line))
			continue
		}

		kv := fmRes().kv.FindStringSubmatch(line)
		if kv == nil {
			currentKey, currentList, haveList = "", nil, false
			continue
		}
		key, value := kv[1], strings.TrimSpace(kv[2])
		currentKey, currentList, haveList = key, nil, false
		fm[key] = parseScalar(value)
		if value == "" {
			currentList, haveList = []string{}, true
			fm[key] = currentList
		}
	}
	return fm, body
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
		for part := range strings.SplitSeq(value[1:len(value)-1], ",") {
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
		n, _ := strconv.Atoi(value)
		return n
	}
	return stripQuotes(value)
}

// stripQuotes trims leading/trailing quote characters (both kinds, repeated).
func stripQuotes(s string) string { return strings.Trim(s, "\"'") }
