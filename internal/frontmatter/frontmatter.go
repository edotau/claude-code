// Package frontmatter parses the YAML-subset frontmatter block (string, bool, int, []string) every
// markdown source in this repo (agents, skills, memory topics) uses ahead of its body.
package frontmatter

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jeduden/mdsmith/pkg/markdown"

	"github.com/edotau/claude-code/internal/textutil"
)

// res lazily compiles the package regexes: every claude-code process links frontmatter via crossgen.
var res = sync.OnceValue(func() (r struct {
	list, kv *regexp.Regexp
}) {
	r.list = regexp.MustCompile(`^\s+-\s+(.+)$`)
	r.kv = regexp.MustCompile(`^(\w[\w-]*):\s*(.*)$`)
	return
})

// Parse parses a YAML subset (string, bool, int, []string) from markdown; returns (frontmatter, body).
func Parse(content string) (map[string]any, string) {
	// BOM/CRLF-normalize first: StripFrontMatter anchors on \n, so a CRLF file would silently drop the frontmatter.
	content = strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	prefix, rest := markdown.StripFrontMatter([]byte(content))
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
				if lm := res().list.FindStringSubmatch(line); lm != nil {
					currentList = append(currentList, stripQuotes(strings.TrimSpace(lm[1])))
					fm[currentKey] = currentList
				}
				continue
			}
			existing, _ := fm[currentKey].(string)
			fm[currentKey] = strings.TrimSpace(existing + " " + strings.TrimSpace(line))
			continue
		}

		kv := res().kv.FindStringSubmatch(line)
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
	if textutil.IsDigits(value) {
		n, _ := strconv.Atoi(value)
		return n
	}
	return stripQuotes(value)
}

// stripQuotes trims leading/trailing quote characters (both kinds, repeated).
func stripQuotes(s string) string { return strings.Trim(s, "\"'") }
