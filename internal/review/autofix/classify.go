package autofix

import (
	"regexp"
	"strconv"
	"strings"
)

// Class names a gate finding kind; Deterministic ones have a mechanical fixer, the rest need a fix leaf.
type Class string

const (
	ClassGofmt         Class = "gofmt"
	ClassMarkdownLong  Class = "markdown-long"
	ClassWriterRows    Class = "writer-rows"
	ClassWorkflowBlock Class = "workflow-block"
	ClassComplexity    Class = "complexity"
	ClassIneffassign   Class = "ineffassign"
	ClassFailingTest   Class = "failing-test"
)

// Finding is one classified row of `agents diff --check` output.
type Finding struct {
	Class      Class
	Gate, Path string
	Line       int
	Rule       string
	Message    string
}

// Deterministic reports whether a formatting, sorting or regeneration fixer can repair the class.
func (c Class) Deterministic() bool {
	switch c {
	case ClassGofmt, ClassMarkdownLong, ClassWriterRows, ClassWorkflowBlock:
		return true
	default:
		return false
	}
}

var (
	reANSI     = regexp.MustCompile(`\x1b\[[0-9;]*m`) // renderGates colors FAIL and skip on a terminal
	reHeader   = regexp.MustCompile(`^(FAIL|ok|skip) (.+?) \((?:[\d.]+s|.+)\)$`)
	reWithPath = regexp.MustCompile(`^(.+?)(?::(\d+))?: ([\w-]+)(?:/([\w-]+))?: (.*)$`)
	reNoPath   = regexp.MustCompile(`^([\w-]+)(?:/([\w-]+))?: (.*)$`)
)

// Classify reads the finding rows under each FAIL gate into classes, in input order; unknown rows are dropped.
func Classify(gateOutput string) []Finding {
	var out []Finding
	gate := ""
	for line := range strings.Lines(gateOutput) {
		line = strings.TrimSpace(reANSI.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "agents diff:") || strings.HasPrefix(line, "verify ") {
			gate = ""
			continue
		}
		if m := reHeader.FindStringSubmatch(line); m != nil {
			gate = ""
			if m[1] == "FAIL" {
				gate = m[2]
			}
			continue
		}
		if gate == "" {
			continue
		}
		if f, ok := classifyLine(gate, line); ok {
			out = append(out, f)
		}
	}
	return out
}

func classifyLine(gate, line string) (Finding, bool) {
	path, lineNum, tool, rule, msg, ok := parseFindingLine(line)
	if !ok {
		return Finding{}, false
	}
	c := pickClass(gate, tool, rule, msg)
	if c == "" {
		return Finding{}, false
	}
	return Finding{Class: c, Gate: gate, Path: path, Line: lineNum, Rule: rule, Message: msg}, true
}

// parseFindingLine splits inspect.Finding.String(): `[path[:line]: ]tool[/rule]: message`.
func parseFindingLine(line string) (path string, lineNum int, tool, rule, msg string, ok bool) {
	if m := reWithPath.FindStringSubmatch(line); m != nil && isPath(m[1]) {
		n, _ := strconv.Atoi(m[2]) // empty when the row has no line number
		return m[1], n, m[3], m[4], m[5], true
	}
	if m := reNoPath.FindStringSubmatch(line); m != nil {
		return "", 0, m[1], m[2], m[3], true
	}
	return "", 0, "", "", "", false
}

// isPath tells a file or package path from a bare tool name such as `test`.
func isPath(s string) bool {
	return strings.ContainsAny(s, "./")
}

type row struct{ gate, tool, rule, msg string }

// classRules is checked in order; the first match wins.
var classRules = []struct {
	class Class
	match func(r row) bool
}{
	{ClassGofmt, func(r row) bool { return r.gate == "gofmt" || r.lint("gofmt") }},
	{ClassMarkdownLong, func(r row) bool { return r.tool == "mdsmith" && r.rule == "MDS001" }},
	{ClassWriterRows, func(r row) bool { return strings.Contains(r.msg, "is duplicate or unsorted") }},
	{ClassWorkflowBlock, func(r row) bool { return strings.Contains(r.msg, "block differs from embedded source") }},
	{ClassComplexity, func(r row) bool { return r.gate == "complexity" }},
	{ClassIneffassign, func(r row) bool { return r.lint("ineffassign") }},
	{ClassFailingTest, func(r row) bool { return r.gate == "test" && strings.HasPrefix(r.msg, "--- FAIL: ") }},
}

func (r row) lint(rule string) bool { return r.tool == "golangci-lint" && r.rule == rule }

func pickClass(gate, tool, rule, msg string) Class {
	r := row{gate, tool, rule, msg}
	for _, c := range classRules {
		if c.match(r) {
			return c.class
		}
	}
	return ""
}
