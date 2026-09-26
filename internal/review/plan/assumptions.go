package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Finding is one assumption-linter hit; field order matches the Python dict literal for JSON parity.
type Finding struct {
	Line     int    `json:"line"`
	Category string `json:"category"`
	Matched  string `json:"matched"`
	Message  string `json:"message"`
	Context  string `json:"context"`
}

// orderedCounts renders {category: count} preserving first-seen order, like a Python dict.
type orderedCounts struct {
	keys []string
	vals []int
}

func (o orderedCounts) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		vb, _ := json.Marshal(o.vals[i])
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// AssumptionResult mirrors the Python result dict's key order exactly.
type AssumptionResult struct {
	Status        string        `json:"status"`
	Source        string        `json:"source"`
	TotalFindings int           `json:"total_findings"`
	ByCategory    orderedCounts `json:"by_category"`
	Verdict       string        `json:"verdict"`
	Findings      []Finding     `json:"findings"`
}

type signalDef struct {
	category string
	message  string
	match    func(s string) (string, bool)
}

func simpleMatcher(re *regexp.Regexp) func(string) (string, bool) {
	return func(s string) (string, bool) {
		loc := re.FindStringIndex(s)
		if loc == nil {
			return "", false
		}
		return s[loc[0]:loc[1]], true
	}
}

var (
	vagueActionKeywordRe     = regexp.MustCompile(`(?i)\b(?:fix|improve|optimize|refactor|update|enhance)\b`)
	vagueActionExceptionRe   = regexp.MustCompile(`(?i)^\s+\w+\s+(?:to|so|by)\b`)
	unscopedSubjectKeywordRe = regexp.MustCompile(`(?i)\b(?:the\s+user|users)\b`)
	unscopedSubjectExceptRe  = regexp.MustCompile(`(?i)\b(?:who|which|admin|role|authenticated|specific)\b`)
)

// vagueActionMatch replicates `\b(keyword)\b(?!\s+\w+\s+(?:to|so|by))` — RE2 has no lookahead, so this
// walks each keyword occurrence and rejects it if a specific target ("... to/so/by") follows immediately.
func vagueActionMatch(s string) (string, bool) {
	for _, m := range vagueActionKeywordRe.FindAllStringIndex(s, -1) {
		rest := s[m[1]:]
		if !vagueActionExceptionRe.MatchString(rest) {
			return s[m[0]:m[1]], true
		}
	}
	return "", false
}

// unscopedSubjectMatch replicates `\b(the user|users)\b(?!.*\b(who|...)\b)` without RE2 lookahead:
// reject an occurrence if any scoping word appears anywhere later on the line.
func unscopedSubjectMatch(s string) (string, bool) {
	for _, m := range unscopedSubjectKeywordRe.FindAllStringIndex(s, -1) {
		rest := s[m[1]:]
		if !unscopedSubjectExceptRe.MatchString(rest) {
			return s[m[0]:m[1]], true
		}
	}
	return "", false
}

var signals = []signalDef{
	{
		"minimizing",
		"Minimizing language often hides complexity. What's being skipped?",
		simpleMatcher(regexp.MustCompile(`(?i)\b(?:just|simply|simple|straightforward|trivial|easy)\b`)),
	},
	{
		"unstated-assumption",
		"Signals an unstated assumption. Is it really obvious?",
		simpleMatcher(regexp.MustCompile(`(?i)\b(?:obviously|clearly|of course|naturally)\b`)),
	},
	{
		"hopeful",
		"Hopeful, not verified. How will you confirm?",
		simpleMatcher(regexp.MustCompile(`(?i)\b(?:should\s+(?:be\s+fine|work)|shouldn't\s+be\s+a\s+problem|probably)\b`)),
	},
	{
		"explicit-assumption",
		"Explicit — good — but have you verified it?",
		simpleMatcher(regexp.MustCompile(`(?i)\b(?:I\s+assume|assuming|I'm\s+guessing)\b`)),
	},
	{
		"absolute-scope",
		"Absolute scope. Is that really the case for every input?",
		simpleMatcher(regexp.MustCompile(`(?i)\b(?:all\s+users|every|everything|always|never)\b`)),
	},
	{
		"vague-action",
		"Vague action verb. What specifically changes? What's the measurable result?",
		vagueActionMatch,
	},
	{
		"unscoped-subject",
		"Which user(s)? All? A specific role? Authenticated only?",
		unscopedSubjectMatch,
	},
}

var (
	stepBlockRe  = regexp.MustCompile(`(?:^|\n)((?:[ \t]*\d+[.)]\s+.+\n?)+)`)
	verifyWordRe = regexp.MustCompile(`(?i)\b(?:test|verify|check|assert|confirm|ensure|validate)\b`)
)

func lintText(text string) []Finding {
	findings := []Finding{}
	for i, line := range splitLines(text) {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		for _, sig := range signals {
			if matched, ok := sig.match(stripped); ok {
				findings = append(findings, Finding{
					Line:     i + 1,
					Category: sig.category,
					Matched:  matched,
					Message:  sig.message,
					Context:  truncateRunes(stripped, 120),
				})
			}
		}
	}

	for _, m := range stepBlockRe.FindAllStringSubmatch(text, -1) {
		block := m[1]
		if verifyWordRe.MatchString(block) {
			continue
		}
		trimmed := strings.ReplaceAll(strings.TrimSpace(block), "\n", " ")
		findings = append(findings, Finding{
			Line:     0,
			Category: "missing-verification",
			Matched:  truncateRunes(trimmed, 80),
			Message:  "Plan block has no verification step. Add a 'verify:' check to each step.",
			Context:  truncateRunes(trimmed, 120),
		})
	}
	return findings
}

type categoryGroup struct {
	name  string
	items []Finding
}

func groupByCategory(findings []Finding) []categoryGroup {
	order := []string{}
	byName := map[string][]Finding{}
	for _, f := range findings {
		if _, seen := byName[f.Category]; !seen {
			order = append(order, f.Category)
		}
		byName[f.Category] = append(byName[f.Category], f)
	}
	groups := make([]categoryGroup, 0, len(order))
	for _, name := range order {
		groups = append(groups, categoryGroup{name, byName[name]})
	}
	return groups
}

// RunAssumptions is the Go entry point for assumption_linter.py's CLI.
func RunAssumptions(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	input, jsonOut, code, handled := parseInputJSONArgs("assumption_linter.py", args, stdout, stderr)
	if handled {
		return code
	}
	text, source, ok := readInput(input, stdin, stderr)
	if !ok {
		return 0
	}

	findings := lintText(text)
	groups := groupByCategory(findings)
	counts := orderedCounts{}
	for _, g := range groups {
		counts.keys = append(counts.keys, g.name)
		counts.vals = append(counts.vals, len(g.items))
	}

	verdict := "CLEAN"
	if len(findings) > 0 {
		if len(findings) < 5 {
			verdict = "REVIEW"
		} else {
			verdict = "CLARIFY"
		}
	}

	result := AssumptionResult{
		Status:        "ok",
		Source:        source,
		TotalFindings: len(findings),
		ByCategory:    counts,
		Verdict:       verdict,
		Findings:      findings,
	}

	if jsonOut {
		b, _ := marshalPyJSON(result)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	fmt.Fprintf(stdout, "Assumption Linter — %s\n", source)
	fmt.Fprintf(stdout, "Findings: %d   Verdict: %s\n", len(findings), verdict)
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "\n  Plan looks explicit. Assumptions are surfaced.")
		return 0
	}
	fmt.Fprintln(stdout)
	for _, g := range groups {
		fmt.Fprintf(stdout, "  [%s] (%d)\n", g.name, len(g.items))
		shown := g.items
		if len(shown) > 5 {
			shown = shown[:5]
		}
		for _, item := range shown {
			ref := ""
			if item.Line != 0 {
				ref = fmt.Sprintf("L%d: ", item.Line)
			}
			fmt.Fprintf(stdout, "    %s%s\n", ref, item.Message)
			fmt.Fprintf(stdout, "      -> \"%s\" in: %s\n", item.Matched, truncateRunes(item.Context, 80))
		}
		if len(g.items) > 5 {
			fmt.Fprintf(stdout, "    ... and %d more\n", len(g.items)-5)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}
