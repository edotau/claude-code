package contract

import (
	"fmt"
	"strconv"
	"strings"
)

// ReportFields is a fresh copy of the checklist shared by the brief and report-term check.
func ReportFields() []string {
	return []string{"Outcome", "Commands and exit codes", "Observable proof", "Test teeth proof",
		"Gross production lines added and deleted", "What net growth clarified", "Anything blocked"}
}

// ReportInstructions supplies contract briefs with the checklist; Go checks completeness, not truth.
func ReportInstructions() string {
	return "Report: use each of these exact level-two headings, with a non-empty body:\n## " +
		strings.Join(ReportFields(), "\n## ") +
		"\nUnder gross production lines, write Added: <nonnegative integer> and Deleted: <nonnegative integer>" +
		" on separate lines. Exclude tests, scratch receipts, generated files and documentation from production counts;" +
		" list those separately. Explain what growth clarified (or why there was none)." +
		" Test teeth proof must explain the failure caught, or justify why tests do not apply." +
		" Complete both the requested Report path and the report filed with Go if they differ."
}

// ReportProblems checks non-empty sections and nonnegative production counts in the actual report.
func ReportProblems(body string) []string {
	sections := reportSections(body)
	var problems []string
	for _, field := range ReportFields() {
		if strings.TrimSpace(sections[field]) == "" {
			problems = append(problems, field)
		}
	}
	counts := sections["Gross production lines added and deleted"]
	for _, key := range []string{"Added", "Deleted"} {
		if !productionCount(counts, key) {
			problems = append(problems, fmt.Sprintf("production %s count", key))
		}
	}
	return problems
}

func reportSections(body string) map[string]string {
	sections := map[string]string{}
	var heading, fence string
	var content strings.Builder
	flush := func() {
		if heading != "" {
			sections[heading] += content.String()
		}
		content.Reset()
	}
	for line := range strings.SplitSeq(body, "\n") {
		if next, marker := reportFenceState(line, fence); marker {
			fence = next
			continue
		}
		if name, ok := reportHeading(line); fence == "" && ok {
			flush()
			heading = name
			continue
		}
		content.WriteString(line + "\n")
	}
	flush()
	return sections
}

func reportHeading(line string) (string, bool) {
	line = markdownLine(line)
	if name, ok := strings.CutPrefix(line, "## "); ok {
		name = strings.TrimSpace(name)
		if trimmed := strings.TrimRight(name, "#"); trimmed != name && strings.HasSuffix(trimmed, " ") {
			name = strings.TrimSpace(trimmed)
		}
		return name, true
	}
	return "", strings.HasPrefix(line, "# ")
}

func markdownLine(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return ""
	}
	return trimmed
}

func reportFenceState(line, fence string) (string, bool) {
	delimiter, closing := reportFence(line)
	if delimiter == "" {
		return fence, false
	}
	if fence == "" {
		return delimiter, true
	}
	if closing && strings.HasPrefix(delimiter, fence) {
		return "", true
	}
	return fence, false
}

func reportFence(line string) (string, bool) {
	line = markdownLine(line)
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return "", false
	}
	n := len(line) - len(strings.TrimLeft(line, string(line[0])))
	if n < 3 || line[0] == '`' && strings.Contains(line[n:], "`") {
		return "", false
	}
	return line[:n], strings.TrimSpace(line[n:]) == ""
}

func productionCount(body, key string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "), "+ "))
		if name, value, ok := strings.Cut(line, ":"); ok && strings.Trim(name, "*_` ") == key {
			n, err := strconv.Atoi(strings.Trim(value, "*_` "))
			return err == nil && n >= 0
		}
	}
	return false
}
