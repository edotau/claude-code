package source

import (
	"fmt"
	"strings"
)

// analyzePython is complexity_checker's `analyze_python` PORTED AS AN ESTIMATE: Go has no Python
// parser (go/ast is Go-only), so this scans indentation + control-flow keywords instead of a real AST.
// Deviation from the Python original (which uses ast, exact per-function metrics): function order is
// top-to-bottom source order (ast.walk is BFS), and boolean-operator scoring follows the SonarSource
// "operator-sequence-change" rule rather than CPython's `len(BoolOp.values)-1`.
func analyzePython(text string, lineCount int, th ComplexityThresholds) []Finding {
	lines := pythonLines(maskPythonSource(text))
	var findings []Finding
	for _, f := range findPyFunctions(lines) {
		findings = append(findings, pyFunctionFindings(f, th)...)
	}
	findings = append(findings, pyModuleFindings(lines, text, th, lineCount)...)
	return findings
}

type pyLine struct {
	indent       int
	text         string
	lineNo       int
	continuation bool
}

func skipLine(l pyLine) bool { return l.continuation || strings.TrimSpace(l.text) == "" }

// pythonLines splits masked source into indent-tagged lines, flagging backslash/bracket continuations.
func pythonLines(masked string) []pyLine {
	raw := strings.Split(masked, "\n")
	out := make([]pyLine, len(raw))
	depth, prevBackslash := 0, false
	for i, l := range raw {
		out[i] = pyLine{indent: leadingWidth(l), text: l, lineNo: i + 1, continuation: depth > 0 || prevBackslash}
		prevBackslash = strings.HasSuffix(strings.TrimRight(l, " \t"), "\\")
		for _, r := range l {
			switch r {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return out
}

// leadingWidth counts indentation columns (a tab advances to the next multiple of 8, Python's tab rule).
func leadingWidth(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 8 - (n % 8)
		default:
			return n
		}
	}
	return n
}

type pyFuncSpan struct {
	name               string
	startLine, endLine int
	bodyLines          []pyLine
}

// findPyFunctions locates `def`/`async def` lines and spans each to the next same-or-lower indent line.
func findPyFunctions(lines []pyLine) []pyFuncSpan {
	var funcs []pyFuncSpan
	for i, l := range lines {
		if skipLine(l) {
			continue
		}
		name, ok := pyDefName(strings.TrimLeft(l.text, " \t"))
		if !ok {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if skipLine(lines[j]) {
				continue
			}
			if lines[j].indent <= l.indent {
				end = j
				break
			}
		}
		// Trim trailing blank/comment lines: ast's end_lineno is the last real statement, not the gap
		// before the next sibling def (matters for trailing blank lines before a dedent).
		last := end - 1
		for last > i && skipLine(lines[last]) {
			last--
		}
		funcs = append(funcs, pyFuncSpan{name: name, startLine: l.lineNo, endLine: lines[last].lineNo, bodyLines: lines[i+1 : end]})
	}
	return funcs
}

// pyDefName extracts the function name from a `def foo(...)`/`async def foo(...)` line.
func pyDefName(trimmed string) (string, bool) {
	s := trimmed
	switch {
	case strings.HasPrefix(s, "async def "):
		s = s[len("async def "):]
	case strings.HasPrefix(s, "def "):
		s = s[len("def "):]
	default:
		return "", false
	}
	end := strings.IndexAny(s, "( \t")
	if end <= 0 {
		return "", false
	}
	return s[:end], true
}

var pyCycloKeywords = map[string]bool{"if": true, "elif": true, "for": true, "while": true, "except": true, "with": true, "assert": true}

// pyFuncCyclomatic estimates cyclomatic complexity: 1 + a keyword per branch + boolop sequence changes.
func pyFuncCyclomatic(body []pyLine) int {
	score := 1
	for _, l := range body {
		if skipLine(l) {
			continue
		}
		words := pyWords(l.text)
		score += pyBranchKeywordCount(words)
		score += pyBoolOpTransitions(words)
	}
	return score
}

func pyBranchKeywordCount(words []string) int {
	n := 0
	for _, w := range words {
		if pyCycloKeywords[w] {
			n++
		}
	}
	return n
}

// pyBoolOpTransitions counts and<->or operator switches within one line (SonarSource cognitive-complexity rule).
func pyBoolOpTransitions(words []string) int {
	n, prev := 0, ""
	for _, w := range words {
		if w != "and" && w != "or" {
			continue
		}
		if prev != "" && prev != w {
			n++
		}
		prev = w
	}
	return n
}

func pyWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

// pyFuncNesting estimates max block-nesting depth via an indent stack (function itself = 0).
func pyFuncNesting(body []pyLine) int {
	var stack []int
	maxDepth := 0
	for _, l := range body {
		if skipLine(l) {
			continue
		}
		for len(stack) > 0 && l.indent <= stack[len(stack)-1] {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > maxDepth {
			maxDepth = len(stack)
		}
		if pyOpensNesting(strings.TrimLeft(l.text, " \t")) {
			stack = append(stack, l.indent)
		}
	}
	return maxDepth
}

var pyNestingPrefixes = []string{"if ", "if(", "elif ", "for ", "async for ", "while ", "try:", "try ", "with ", "async with ", "match "}

func pyOpensNesting(trimmed string) bool {
	for _, p := range pyNestingPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// pyFunctionFindings is check_function's estimate: length, cyclomatic complexity, nesting depth.
func pyFunctionFindings(f pyFuncSpan, th ComplexityThresholds) []Finding {
	var out []Finding
	if length := f.endLine - f.startLine + 1; length > th.MaxFunctionLines {
		out = append(out, Finding{Rule: "function-length", Severity: "warn", Line: f.startLine,
			Message: fmt.Sprintf("'%s' is %d lines (max %d). Split it.", f.name, length, th.MaxFunctionLines)})
	}
	if cyclo := pyFuncCyclomatic(f.bodyLines); cyclo > th.MaxCyclomatic {
		out = append(out, Finding{Rule: "cyclomatic-complexity", Severity: "warn", Line: f.startLine,
			Message: fmt.Sprintf("'%s' has cyclomatic complexity %d (max %d). Flatten branching.", f.name, cyclo, th.MaxCyclomatic)})
	}
	if nest := pyFuncNesting(f.bodyLines); nest > th.MaxNesting {
		out = append(out, Finding{Rule: "nesting-depth", Severity: "warn", Line: f.startLine,
			Message: fmt.Sprintf("'%s' nests %d levels deep (max %d). Use early returns.", f.name, nest, th.MaxNesting)})
	}
	return out
}

// pyModuleFindings is check_file_shape's estimate: import count, class density, premature abstraction.
func pyModuleFindings(lines []pyLine, rawText string, th ComplexityThresholds, lineCount int) []Finding {
	imports, classes := 0, 0
	for _, l := range lines {
		if skipLine(l) {
			continue
		}
		t := strings.TrimLeft(l.text, " \t")
		if l.indent == 0 && (strings.HasPrefix(t, "import ") || strings.HasPrefix(t, "from ")) {
			imports++
		}
		if strings.HasPrefix(t, "class ") {
			classes++
		}
	}
	var out []Finding
	if imports > th.MaxImports {
		out = append(out, Finding{Rule: "import-count", Severity: "warn",
			Message: fmt.Sprintf("%d imports (max %d). High coupling?", imports, th.MaxImports)})
	}
	if lineCount > 0 {
		if density := float64(classes) / (float64(lineCount) / 100); density > th.MaxClassesPer100Lines {
			out = append(out, Finding{Rule: "class-density", Severity: "warn",
				Message: fmt.Sprintf("%d classes in %d lines (%.1f/100). Premature abstraction?", classes, lineCount, density)})
		}
	}
	if classes > 0 && lineCount < 200 && pyHasABCMarker(rawText) {
		out = append(out, Finding{Rule: "premature-abstraction", Severity: "info",
			Message: "Abstract base / Protocol in a file under 200 lines. Needed yet?"})
	}
	return out
}

func pyHasABCMarker(text string) bool {
	for _, m := range []string{"abstractmethod", "ABCMeta", "(ABC", ", ABC", "Protocol"} {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
