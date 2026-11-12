package source

import (
	"strings"
)

// analyzePython is complexity_checker's `analyze_python` PORTED AS AN ESTIMATE: Go has no Python
// parser (go/ast is Go-only), so this scans indentation + control-flow keywords instead of a real AST.
// Deviation from the Python original (which uses ast, exact per-function metrics): function order is
// top-to-bottom source order (ast.walk is BFS), and boolean-operator scoring follows the SonarSource
// "operator-sequence-change" rule rather than CPython's `len(BoolOp.values)-1`.
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

// pyMaskState is the byte state machine used by maskPythonSource.
type pyMaskState int

const (
	pyNormal pyMaskState = iota
	pyComment
	pySingle
	pyDouble
	pyTripleSingle
	pyTripleDouble
)

// maskPythonSource blanks string/comment interiors (one-pass byte scan) so keyword counting downstream
// never sees a keyword that lives inside a string literal or a # comment. Structure (indent, newlines,
// parens/brackets) is preserved so line numbers and continuation tracking stay accurate.
func maskPythonSource(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	state := pyNormal
	for i := 0; i < len(b); {
		switch state {
		case pyNormal:
			i, state = pyMaskNormal(b, out, i, &state)
		case pyComment:
			if b[i] == '\n' {
				state = pyNormal
			} else {
				maskByte(out, i)
			}
			i++
		default:
			i = pyMaskInString(b, out, i, &state)
		}
	}
	return string(out)
}

// pyMaskNormal handles one byte outside any string/comment; may switch state and/or skip ahead.
func pyMaskNormal(b, out []byte, i int, state *pyMaskState) (int, pyMaskState) {
	c := b[i]
	switch {
	case c == '#':
		*state = pyComment
		out[i] = ' '
		return i + 1, *state
	case (c == '\'' || c == '"') && tripleAt(b, i, c):
		*state = tripleState(c)
		blankN(out, i, 3)
		return i + 3, *state
	case c == '\'':
		*state = pySingle
	case c == '"':
		*state = pyDouble
	}
	return i + 1, *state
}

// pyMaskInString handles one byte inside a (possibly triple) quoted string, honoring backslash escapes.
func pyMaskInString(b, out []byte, i int, state *pyMaskState) int {
	c := b[i]
	triple := *state == pyTripleSingle || *state == pyTripleDouble
	quote := pyStateQuote(*state)
	if c == '\\' {
		maskByte(out, i)
		if i+1 < len(b) {
			maskByte(out, i+1)
		}
		return i + 2
	}
	if triple && tripleAt(b, i, quote) {
		*state = pyNormal
		blankN(out, i, 3)
		return i + 3
	}
	if !triple && (c == quote || c == '\n') {
		*state = pyNormal
		return i + 1
	}
	maskByte(out, i)
	return i + 1
}

func pyStateQuote(state pyMaskState) byte {
	if state == pyDouble || state == pyTripleDouble {
		return '"'
	}
	return '\''
}

// maskByte blanks one byte, but a newline is structure (line numbers), never string/comment content.
func maskByte(out []byte, i int) {
	if out[i] != '\n' {
		out[i] = ' '
	}
}

func tripleState(q byte) pyMaskState {
	if q == '\'' {
		return pyTripleSingle
	}
	return pyTripleDouble
}

// tripleAt reports whether b[i:i+3] is q,q,q (a triple-quote delimiter).
func tripleAt(b []byte, i int, q byte) bool {
	return i+2 < len(b) && b[i] == q && b[i+1] == q && b[i+2] == q
}

// blankN overwrites n bytes starting at i with spaces, preserving any embedded newline.
func blankN(out []byte, i, n int) {
	for k := 0; k < n && i+k < len(out); k++ {
		if out[i+k] != '\n' {
			out[i+k] = ' '
		}
	}
}
