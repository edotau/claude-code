package planlint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"sync"

	reviewsource "github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// finding is one assumption-linter hit; field order matches the Python dict literal for JSON parity.
type finding struct {
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

// assumptionResult mirrors the Python result dict's key order exactly.
type assumptionResult struct {
	Status        string        `json:"status"`
	Source        string        `json:"source"`
	TotalFindings int           `json:"total_findings"`
	ByCategory    orderedCounts `json:"by_category"`
	Verdict       string        `json:"verdict"`
	Findings      []finding     `json:"findings"`
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
	vagueActionKeywordRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:fix|improve|optimize|refactor|update|enhance)\b`)
	})
	vagueActionExceptionRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^\s+\w+\s+(?:to|so|by)\b`) })
	unscopedSubjectKeywordRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\b(?:the\s+user|users)\b`) })
	unscopedSubjectExceptRe  = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:who|which|admin|role|authenticated|specific)\b`)
	})
)

// matchUnlessFollowed emulates a negative lookahead (RE2 has none): the first keyword hit not followed by except.
func matchUnlessFollowed(keyword, except *regexp.Regexp, s string) (string, bool) {
	for _, m := range keyword.FindAllStringIndex(s, -1) {
		if !except.MatchString(s[m[1]:]) {
			return s[m[0]:m[1]], true
		}
	}
	return "", false
}

// vagueActionMatch replicates `\b(keyword)\b(?!\s+\w+\s+(?:to|so|by))`: a specific target ("... to/so/by") may follow.
func vagueActionMatch(s string) (string, bool) {
	return matchUnlessFollowed(vagueActionKeywordRe(), vagueActionExceptionRe(), s)
}

// unscopedSubjectMatch replicates `\b(the user|users)\b(?!.*\b(who|...)\b)`: a scoping word may follow anywhere.
func unscopedSubjectMatch(s string) (string, bool) {
	return matchUnlessFollowed(unscopedSubjectKeywordRe(), unscopedSubjectExceptRe(), s)
}

var signals = sync.OnceValue(func() []signalDef {
	return []signalDef{
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
})

var (
	// A step block is numbered lines plus the indented, non-numbered continuation lines under them.
	stepBlockRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?:^|\n)((?:[ \t]*\d+[.)]\s+.+\n?)(?:(?:[ \t]*\d+[.)]\s+.+|[ \t]+[^ \t\n\d].*)\n?)*)`)
	})
	verifyWordRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:test|verify|check|assert|confirm|ensure|validate)\b`)
	})
)

func lintText(text string) []finding {
	findings := []finding{}
	for i, line := range splitLines(text) {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		for _, sig := range signals() {
			if matched, ok := sig.match(stripped); ok {
				findings = append(findings, finding{
					Line:     i + 1,
					Category: sig.category,
					Matched:  matched,
					Message:  sig.message,
					Context:  textutil.TruncateRunes(stripped, 120),
				})
			}
		}
	}

	for _, m := range stepBlockRe().FindAllStringSubmatch(text, -1) {
		block := m[1]
		if verifyWordRe().MatchString(block) {
			continue
		}
		trimmed := strings.ReplaceAll(strings.TrimSpace(block), "\n", " ")
		findings = append(findings, finding{
			Line:     0,
			Category: "missing-verification",
			Matched:  textutil.TruncateRunes(trimmed, 80),
			Message:  "Plan block has no verification step. Add a 'verify:' check to each step.",
			Context:  textutil.TruncateRunes(trimmed, 120),
		})
	}
	return findings
}

type categoryGroup struct {
	name  string
	items []finding
}

func groupByCategory(findings []finding) []categoryGroup {
	order := []string{}
	byName := map[string][]finding{}
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
	input, jsonOut, code, handled := parseInputJSONArgs("assumptions",
		"Lint a plan for vague actions, unscoped subjects and hopeful steps.", args, stdout, stderr)
	if handled {
		return code
	}
	text, source, exit, ok := readInput(input, stdin, stderr)
	if !ok {
		return exit
	}

	findings := lintText(text)
	groups := groupByCategory(findings)
	counts := orderedCounts{}
	for _, g := range groups {
		counts.keys = append(counts.keys, g.name)
		counts.vals = append(counts.vals, len(g.items))
	}

	verdict := assumptionVerdict(len(findings))

	var err error
	if jsonOut {
		err = reviewsource.WritePyJSON(stdout, assumptionResult{
			Status:        "ok",
			Source:        source,
			TotalFindings: len(findings),
			ByCategory:    counts,
			Verdict:       verdict,
			Findings:      findings,
		})
	} else {
		err = printAssumptions(stdout, source, findings, groups, verdict)
	}
	if err != nil {
		fmt.Fprintf(stderr, "claude-code review assumptions: error: writing output: %v\n", err)
		return 1
	}
	return 0
}

func assumptionVerdict(n int) string {
	switch {
	case n == 0:
		return "CLEAN"
	case n < 5:
		return "REVIEW"
	}
	return "CLARIFY"
}

func printAssumptions(dst io.Writer, source string, findings []finding, groups []categoryGroup, verdict string) error {
	stdout := &strings.Builder{}
	fmt.Fprintf(stdout, "Assumption Linter — %s\n", source)
	fmt.Fprintf(stdout, "Findings: %d   Verdict: %s\n", len(findings), verdict)
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "\n  Plan looks explicit. Assumptions are surfaced.")
	} else {
		fmt.Fprintln(stdout)
		for _, g := range groups {
			fmt.Fprintf(stdout, "  [%s] (%d)\n", g.name, len(g.items))
			for _, item := range g.items[:min(len(g.items), 5)] {
				ref := ""
				if item.Line != 0 {
					ref = fmt.Sprintf("L%d: ", item.Line)
				}
				fmt.Fprintf(stdout, "    %s%s\n", ref, item.Message)
				fmt.Fprintf(stdout, "      -> \"%s\" in: %s\n", item.Matched, textutil.TruncateRunes(item.Context, 80))
			}
			if len(g.items) > 5 {
				fmt.Fprintf(stdout, "    ... and %d more\n", len(g.items)-5)
			}
			fmt.Fprintln(stdout)
		}
	}
	_, err := io.WriteString(dst, stdout.String())
	return err
}

var lineSplitRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\r\n|\r|\n`) })

// splitLines mirrors Python's str.splitlines() for the line endings these scripts see in practice.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return lineSplitRe().Split(text, -1)
}

// readInput reads "-" (stdin) or a path; a missing path or any read failure exits 1 so a gate never passes unlinted.
func readInput(input string, stdin io.Reader, stderr io.Writer) (text, source string, exit int, ok bool) {
	if input == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "[error] reading stdin: %v\n", err)
			return "", "", 1, false
		}
		return string(b), "stdin", 0, true
	}
	b, err := os.ReadFile(input)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "[error] %s not found\n", input)
		return "", "", 1, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "[error] reading %s: %v\n", input, err)
		return "", "", 1, false
	}
	return string(b), input, 0, true
}

// parseInputJSONArgs parses `review <tool> [<input>] [--json]`; -h anywhere prints usage (exit 0), a bad flag exits 2.
func parseInputJSONArgs(tool, about string, args []string, stdout, stderr io.Writer) (input string, jsonOut bool, exitCode int, handled bool) {
	fs := helptext.NewFlagSet("review "+tool, helptext.Page{
		Usage: "claude-code review " + tool + " [<input>] [flags]",
		About: about + " <input> is a file, or - (the default) for stdin.",
	})
	jsonFlag := fs.Bool("json", false, "Output in JSON format")
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return "-", false, 0, true
	}
	fs.SetOutput(stderr)
	positional, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return "-", false, helptext.ExitCode(err), true
	}
	input = "-"
	switch len(positional) {
	case 0:
	case 1:
		input = positional[0]
	default:
		fmt.Fprintf(stderr, "review %s: unexpected argument %q\n", tool, positional[1])
		fs.Usage()
		return "-", false, 2, true
	}
	return input, *jsonFlag, 0, false
}
