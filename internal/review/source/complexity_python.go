package source

import (
	"strings"
)

// analyzePython is complexity_checker's `analyze_python` PORTED AS AN ESTIMATE: Go has no Python
// parser (go/ast is Go-only), so this scans indentation + control-flow keywords instead of a real AST.
// Deviation from the Python original (which uses ast, exact per-function metrics): function order is
// top-to-bottom source order (ast.walk is BFS).
func analyzePython(text string, lineCount int, th ComplexityThresholds) []Finding {
	masked := maskPythonSource(text)
	lines := pythonLines(masked)
	var findings []Finding
	for _, f := range findPyFunctions(lines) {
		findings = append(findings, pyFunctionFindings(f, th)...)
	}
	findings = append(findings, pyModuleFindings(lines, masked, th, lineCount)...)
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
		prevBackslash = strings.HasSuffix(strings.TrimRight(l, " \t\r"), "\\")
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

// pyFuncCyclomatic estimates cyclomatic complexity: 1 + a keyword per branch + one per and/or operator.
func pyFuncCyclomatic(body []pyLine) int {
	score := 1
	for _, l := range body {
		if skipLine(l) {
			continue
		}
		words := pyWords(l.text)
		score += pyBranchKeywordCount(words)
		score += pyBoolOpCount(words)
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

// pyBoolOpCount counts every and/or operator on a line, matching the original's sum of len(BoolOp.values)-1.
func pyBoolOpCount(words []string) int {
	n := 0
	for _, w := range words {
		if w == "and" || w == "or" {
			n++
		}
	}
	return n
}

func pyWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
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
		if pyOpensNesting(strings.TrimLeft(l.text, " \t")) {
			stack = append(stack, l.indent)
			maxDepth = max(maxDepth, len(stack)) // counted on push so an inline suite (`for x in xs: f()`) counts
		}
	}
	return maxDepth
}

var pyNestingPrefixes = []string{"if ", "if(", "elif ", "for ", "async for ", "while ", "try:", "try ", "with ", "async with ", "else:", "except ", "except:", "finally:"}

func pyOpensNesting(trimmed string) bool {
	if (strings.HasPrefix(trimmed, "case ") || strings.HasPrefix(trimmed, "match ")) && strings.HasSuffix(strings.TrimRight(trimmed, " \t"), ":") {
		return true // soft keywords: `match = f()` is an assignment, so the trailing colon decides
	}
	for _, p := range pyNestingPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// pyFunctionFindings is check_function's estimate: length, cyclomatic complexity, nesting depth.
func pyFunctionFindings(f pyFuncSpan, th ComplexityThresholds) []Finding {
	return functionFindings(f.name, f.startLine, f.endLine-f.startLine+1, pyFuncCyclomatic(f.bodyLines), pyFuncNesting(f.bodyLines), th)
}

// pyModuleFindings is check_file_shape's estimate: import count, class density, premature abstraction.
func pyModuleFindings(lines []pyLine, maskedText string, th ComplexityThresholds, lineCount int) []Finding {
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
	out := importCountFinding(imports, th)
	out = append(out, classDensityFinding(classes, lineCount, th)...)
	if classes > 0 && lineCount < 200 && pyHasABCMarker(maskedText) {
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

// maskPythonSource blanks string/comment interiors, keeping indent, newlines and brackets so line tracking holds.
func maskPythonSource(src string) string {
	out := []byte(src)
	for i := 0; i < len(src); {
		c := src[i]
		if c != '#' && c != '\'' && c != '"' {
			i++
			continue
		}
		end, closed := pyLiteralEnd(src, i)
		lo, hi := i, end
		if c != '#' && !tripleAt(src, i, c) { // a single-quoted string keeps its own quote characters
			lo = i + 1
			if closed {
				hi = end - 1
			}
		}
		for k := lo; k < hi; k++ {
			maskByte(out, k)
		}
		i = end
	}
	return string(out)
}

// pyLiteralEnd scans the comment or string opened at s[i]; it returns the index past it and whether a quote closed it.
// A comment and an unclosed single-quoted string stop before the newline; a backslash escapes the next byte.
func pyLiteralEnd(s string, i int) (end int, closed bool) {
	q := s[i]
	if q == '#' {
		return pyCommentEnd(s, i), true
	}
	return pyStringEnd(s, i, q)
}

// pyStringEnd scans the single- or triple-quoted string opened at s[i] with quote q.
func pyStringEnd(s string, i int, q byte) (end int, closed bool) {
	n := 1
	if tripleAt(s, i, q) {
		n = 3
	}
	for j := i + n; j < len(s); j++ {
		switch {
		case s[j] == '\\':
			j++
		case n == 3 && tripleAt(s, j, q):
			return j + 3, true
		case n == 1 && s[j] == q:
			return j + 1, true
		case n == 1 && s[j] == '\n':
			return j, false
		}
	}
	return len(s), false
}

// pyCommentEnd returns the index of the newline ending the comment opened at s[i], or len(s).
func pyCommentEnd(s string, i int) int {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return i + nl
	}
	return len(s)
}

// maskByte blanks one byte, but a newline is structure (line numbers), never string/comment content.
func maskByte(out []byte, i int) {
	if out[i] != '\n' {
		out[i] = ' '
	}
}

// tripleAt reports whether s[i:i+3] is q,q,q (a triple-quote delimiter).
func tripleAt(s string, i int, q byte) bool {
	return i+2 < len(s) && s[i] == q && s[i+1] == q && s[i+2] == q
}
