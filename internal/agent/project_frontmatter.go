package agent

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/goldmark/parser"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// descRes lazily compiles the package regexes: every claude-code process links crossgen via the cli table.
var descRes = sync.OnceValue(func() (r struct {
	example, ws *regexp.Regexp
}) {
	r.example = regexp.MustCompile(`(?s)<example>.*?</example>`)
	r.ws = regexp.MustCompile(`\s+`)
	return
})

// firstSentence collapses a multi-line description to its first sentence.
func firstSentence(description string) string {
	text := strings.Join(strings.Fields(description), " ")
	if before, _, ok := strings.Cut(text, ". "); ok {
		return before + "."
	}
	return text
}

// cleanDescription strips <example>...</example> blocks and collapses whitespace.
func cleanDescription(raw string) string {
	withoutExamples := descRes().example.ReplaceAllString(raw, "")
	return strings.TrimSpace(descRes().ws.ReplaceAllString(withoutExamples, " "))
}

// stripMemorySection removes the first "## Memory" section (up to the next level-2 heading or EOF), via the AST
// so a "## Memory"-shaped line inside a fenced code block is never mistaken for the real heading.
func stripMemorySection(body string) string {
	src := []byte(body)
	root := markdown.ParseContext(src, parser.NewContext())
	var target *mdast.Heading
	_ = mdast.Walk(root, func(n mdast.Node, entering bool) (mdast.WalkStatus, error) {
		if !entering || target != nil {
			return mdast.WalkContinue, nil
		}
		if h, ok := n.(*mdast.Heading); ok && h.Level == 2 && strings.TrimSpace(string(h.Lines().Value(src))) == "Memory" {
			target = h
			return mdast.WalkStop, nil
		}
		return mdast.WalkContinue, nil
	})
	if target == nil || target.Lines().Len() == 0 {
		return strings.TrimLeft(body, "\n")
	}
	start := lineStart(src, target.Lines().At(0).Start)
	end := len(src)
	for sib := target.NextSibling(); sib != nil; sib = sib.NextSibling() {
		h, ok := sib.(*mdast.Heading)
		if !ok || h.Level != 2 || h.Lines().Len() == 0 {
			continue
		}
		if next := lineStart(src, h.Lines().At(0).Start); next-1 >= start {
			end = next - 1
		}
		break
	}
	edited := markdown.Splice(src, []markdown.Edit{{Start: start, End: end}})
	return strings.TrimLeft(string(edited), "\n")
}

// lineStart returns the byte offset of the start of the line containing pos in src.
func lineStart(src []byte, pos int) int {
	if i := strings.LastIndexByte(string(src[:pos]), '\n'); i >= 0 {
		return i + 1
	}
	return 0
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
