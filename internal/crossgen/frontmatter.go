package crossgen

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// fmRes lazily compiles the package regexes: every claude-code process links crossgen via the cli table.
var fmRes = sync.OnceValue(func() (r struct {
	split, list, kv, example, ws, memory *regexp.Regexp
}) {
	r.split = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n?(.*)$`)
	r.list = regexp.MustCompile(`^\s+-\s+(.+)$`)
	r.kv = regexp.MustCompile(`^(\w[\w-]*):\s*(.*)$`)
	r.example = regexp.MustCompile(`(?s)<example>.*?</example>`)
	r.ws = regexp.MustCompile(`\s+`)
	r.memory = regexp.MustCompile(`(?s)## Memory\n.*?(\n## |$)`)
	return
})

// ParseFrontmatter parses a YAML subset (string, bool, int, []string) from markdown; returns (frontmatter, body).
func ParseFrontmatter(content string) (map[string]any, string) {
	// BOM/CRLF-normalize first: the split regex anchors on \n, so a CRLF file would silently drop the frontmatter.
	content = strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	m := fmRes().split.FindStringSubmatch(content)
	if m == nil {
		return map[string]any{}, content
	}
	body := strings.TrimSpace(m[2])

	fm := map[string]any{}
	currentKey := ""
	var currentList []string
	haveList := false

	for _, line := range strings.Split(m[1], "\n") {
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

// parseScalar converts one frontmatter value: block-scalar marker → "", inline list, bool, int, else string.
func parseScalar(value string) any {
	switch value {
	case "", "|", "|-", ">", ">-":
		return ""
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		items := []string{}
		for _, part := range strings.Split(value[1:len(value)-1], ",") {
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
	if isDigits(value) {
		n, _ := strconv.Atoi(value)
		return n
	}
	return stripQuotes(value)
}

// isDigits mirrors Python str.isdigit(): non-empty and all ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// stripQuotes trims leading/trailing quote characters (both kinds, repeated).
func stripQuotes(s string) string { return strings.Trim(s, "\"'") }

// firstSentence collapses a multi-line description to its first sentence.
func firstSentence(description string) string {
	text := strings.Join(strings.Fields(description), " ")
	if idx := strings.Index(text, ". "); idx >= 0 {
		return text[:idx] + "."
	}
	return text
}

// cleanDescription strips <example>...</example> blocks and collapses whitespace.
func cleanDescription(raw string) string {
	withoutExamples := fmRes().example.ReplaceAllString(raw, "")
	return strings.TrimSpace(fmRes().ws.ReplaceAllString(withoutExamples, " "))
}

// stripMemorySection removes the first "## Memory" section (up to the next "## " or EOF), then left-trims newlines.
func stripMemorySection(body string) string {
	loc := fmRes().memory.FindStringSubmatchIndex(body)
	if loc == nil {
		return strings.TrimLeft(body, "\n")
	}
	trailing := body[loc[2]:loc[3]]
	return strings.TrimLeft(body[:loc[0]]+trailing+body[loc[1]:], "\n")
}

// yamlQuote quotes a YAML string value, escaping embedded backslashes and double quotes.
func yamlQuote(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	return `"` + strings.ReplaceAll(escaped, `"`, `\"`) + `"`
}

// formatYAMLScalar renders a simple scalar for YAML frontmatter.
func formatYAMLScalar(value any) string {
	switch v := value.(type) {
	case bool, int:
		return toString(v)
	case string:
		return yamlQuote(v)
	default:
		return yamlQuote(toString(v))
	}
}

// orderedField is one frontmatter key/value; emit order is explicit.
type orderedField struct {
	Key   string
	Value any
}

// formatYAML renders ordered fields as YAML: scalars, or string lists inline as `[a, b]`.
func formatYAML(fields []orderedField) string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		if list, ok := f.Value.([]string); ok {
			rendered := make([]string, len(list))
			for i, item := range list {
				rendered[i] = formatYAMLScalar(item)
			}
			lines = append(lines, f.Key+": ["+strings.Join(rendered, ", ")+"]")
			continue
		}
		lines = append(lines, f.Key+": "+formatYAMLScalar(f.Value))
	}
	return strings.Join(lines, "\n")
}

// buildFrontmatterFile assembles a Markdown file with YAML frontmatter.
func buildFrontmatterFile(fields []orderedField, body string) string {
	return "---\n" + formatYAML(fields) + "\n---\n\n" + body
}

// truthy reports whether a parsed frontmatter value is truthy (used for the `readonly` flag).
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	default:
		return false
	}
}

// toString coerces a parsed frontmatter value to its string form for emit.
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}
