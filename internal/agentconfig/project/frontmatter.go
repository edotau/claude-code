package project

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// exampleRe lazily compiles the example-block regex: every claude-code process links crossgen via the cli table.
var exampleRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)<example>.*?</example>`) })

// cleanDescription strips <example>...</example> blocks and collapses whitespace.
func cleanDescription(raw string) string {
	return strings.Join(strings.Fields(exampleRe().ReplaceAllString(raw, "")), " ")
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

// Banner prefixes a generated file carries as its first body line; prune only ever deletes files that carry one.
const (
	agentBannerText        = "Synced from .claude/agents/"
	agentSkillBannerPrefix = "<!-- " + agentBannerText // current agent .md projections, legacy agent-as-skill
	codexBannerPrefix      = "# " + agentBannerText
)

var generatedBannerPrefixes = []string{agentSkillBannerPrefix, codexBannerPrefix, docsBannerPrefix}

// projectedSkillNames is the set of link names the skills pass creates; prune's keep-set for ~/.agents/skills.
func projectedSkillNames(skills []Source) map[string]bool {
	out := make(map[string]bool, len(skills))
	for _, s := range skills {
		out[s.Name()] = true
	}
	return out
}

// skillDescription is the cleaned frontmatter description, falling back to the name.
func skillDescription(src Source) string {
	if raw := strings.TrimSpace(toString(src.frontmatter["description"])); raw != "" {
		return cleanDescription(raw)
	}
	return src.Name()
}
