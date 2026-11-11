package quality

// `workflow lint` — flag workflow agent() calls without an explicit model: pin or an effort pin (an
// unpinned leaf inherits the parent session's reasoning tier, a measured cost trap), a pin whose literal
// value isn't a real tier, and the two script shapes that make the saved-workflow registry skip a file
// silently. The `lint:opus-ok` marker exempts the effort and pin-value checks — a missing model: pin is always a hit.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// lintOpusOK marks a deliberate reasoning-tier agent() call (on the line the call closes on).
const lintOpusOK = "lint:opus-ok"

// pinWindow caps the scan when a call's close paren can't be found (unbalanced string content).
// Generous: prompt template literals routinely run into the thousands of characters.
const pinWindow = 20000

// RunWorkflowLint lints every workflows/*.js plus every skills/*/scripts/*.js that opens with `export
// const meta` under root (default: the config dir the caller passes as args[0]; a bare call uses cwd).
// Exit 1 when anything is flagged.
func RunWorkflowLint(args []string, stdout, stderr io.Writer) int {
	root := "."
	if len(args) > 0 && args[0] != "" {
		if args[0] == "-h" || args[0] == "--help" {
			helptext.Fprint(stdout, "usage: claude-code workflow lint [<root>]   (lints <root>/workflows/*.js and <root>/skills/*/scripts/*.js)\n")
			return 0
		}
		root = args[0]
	}
	pattern := filepath.Join(root, "workflows", "*.js")
	files, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(stderr, "workflow lint: glob %s: %v\n", pattern, err)
		return 1
	}
	skills, err := skillScripts(root)
	if err != nil {
		fmt.Fprintf(stderr, "workflow lint: glob skills scripts under %s: %v\n", root, err)
		return 1
	}
	files = append(files, skills...)
	if len(files) == 0 { // a wrong root (e.g. the workflows dir itself) must not pass as "0 clean"
		fmt.Fprintf(stderr, "workflow lint: no scripts in %s (pass the root that contains workflows/)\n", pattern)
		return 1
	}
	parseHits, skipped := workflowParseDefects(readableFiles(files))
	bad := 0
	if skipped != "" { // plan decision: a missing node is a visible skip; the exit code follows the other checks
		fmt.Fprintln(stderr, skipped)
	}
	for _, f := range files {
		bad += lintScript(f, stderr)
	}
	bad += printHits(stderr, parseHits)
	bad += printHits(stderr, workflowHelperDrift(root))
	bad += printHits(stderr, workflowDocDrift(root))
	if bad > 0 {
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "workflow lint: %d script(s) clean\n", len(files)); err != nil {
		fmt.Fprintf(stderr, "workflow lint: writing output: %v\n", err)
		return 1
	}
	return 0
}

// printHits writes each hit to stderr and returns how many there were.
func printHits(stderr io.Writer, hits []string) int {
	for _, hit := range hits {
		fmt.Fprintln(stderr, hit)
	}
	return len(hits)
}

// readableFiles drops files lintScript already reports as unreadable, so node never re-reports them as parse errors.
func readableFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if _, err := os.ReadFile(f); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// lintScript prints every defect in one script to stderr and returns how many it found.
func lintScript(f string, stderr io.Writer) int {
	data, err := os.ReadFile(f)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read file: %v\n", f, err)
		return 1
	}
	hits := append(LintPins(f, data), registryHidingDefects(f)...)
	for _, hit := range hits {
		fmt.Fprintln(stderr, hit)
	}
	return len(hits)
}

// parseCheckJS compiles each file as an AsyncFunction body (the Workflow runtime's shape), `export` stripped.
const parseCheckJS = `const fs = require('fs')
const AF = Object.getPrototypeOf(async function () {}).constructor
let bad = 0
for (const f of process.argv.slice(1)) {
  try { new AF(fs.readFileSync(f, 'utf8').replace(/^export /gm, '')) } catch (e) { bad++; console.log(f + ': ' + e.message) }
}
process.exit(bad ? 1 : 0)`

// lookNode finds node; tests swap it to prove the visible skip.
var lookNode = func() (string, error) { return exec.LookPath("node") }

// workflowParseDefects compiles every script with node; with no node it returns a visible skip line, never a silent pass.
func workflowParseDefects(files []string) (hits []string, skipped string) {
	if len(files) == 0 {
		return nil, ""
	}
	node, err := lookNode()
	if err != nil {
		return nil, "workflow lint: parse check skipped (node not on PATH)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // a hung node never stalls lint
	defer cancel()
	out, err := exec.CommandContext(ctx, node, append([]string{"-e", parseCheckJS}, files...)...).CombinedOutput()
	if err == nil {
		return nil, ""
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if isParseLine(line, files) {
			hits = append(hits, line+" (does not parse)")
		}
	}
	if len(hits) == 0 {
		hits = []string{"workflow lint: node parse check failed: " + err.Error() + ": " + strings.TrimSpace(string(out))}
	}
	return hits, ""
}

// isParseLine reports whether line is parseCheckJS's own "<file>: <message>" output, not node noise.
func isParseLine(line string, files []string) bool {
	for _, f := range files {
		if strings.HasPrefix(line, f+": ") {
			return true
		}
	}
	return false
}

// skillScripts returns every skills/*/scripts/*.js under root that opens with `export const meta` (a
// Workflow script moved out of workflows/) — other scripts under scripts/ are plain helpers, not linted.
func skillScripts(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(root, "skills", "*", "scripts", "*.js"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if isWorkflowScript(f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// isWorkflowScript reports whether f's first non-blank, non-comment line opens `export const meta`;
// an unreadable file counts as one so lintScript reports it instead of skipping it.
func isWorkflowScript(f string) bool {
	data, err := os.ReadFile(f)
	if err != nil {
		return true
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		return strings.HasPrefix(t, "export const meta")
	}
	return false
}

// LintPins returns every agent() pin problem in src; shared by `workflow lint` and the workflow-lint hook.
func LintPins(name string, src []byte) []string {
	s := string(src)
	var out []string
	for _, hit := range unpinnedAgentCalls(name, s) {
		out = append(out, fmt.Sprintf("%s: agent() without model: — the leaf runs on the session tier, or on a"+
			" built-in agentType's own tier (Explore is opus); pin model:'sonnet' even with agentType", hit))
	}
	for _, hit := range effortlessAgentCalls(name, s) {
		out = append(out, fmt.Sprintf("%s: agent() without effort: — pin effort:'low' for mechanical"+
			" leaves, 'medium' for standard, or mark // %s", hit, lintOpusOK))
	}
	out = append(out, badPinValues(name, s)...)
	return out
}

// unpinnedAgentCalls returns "name:line" for each agent( call missing a model: pin. agentType alone is not
// one (a built-in type such as Explore carries its own tier); the opus-ok marker never exempts this check.
func unpinnedAgentCalls(name, src string) []string {
	return agentCallsMissingSrc(name, src, func(window string) bool { return hasTopLevelKey(window, "model") }, false)
}

// effortlessAgentCalls returns "name:line" for each agent( call with no effort: pin — effort is the
// second routing axis (skills/subagent-workflows/SKILL.md → Rules); unpinned runs at session effort, over-costing.
func effortlessAgentCalls(name, src string) []string {
	return agentCallsMissingSrc(name, src, func(window string) bool {
		return hasTopLevelKey(window, "effort")
	}, true)
}

// agentCallsMissingSrc returns "name:line" for each agent() call whose option
// window fails `pinned`. exemptMarked honors the lint:opus-ok marker; pass false for a check D1 says
// the marker must never waive (the model/agentType pin).
func agentCallsMissingSrc(name, src string, pinned func(window string) bool, exemptMarked bool) []string {
	var out []string
	scanAgentCalls(src, func(_, line int, window string, marked bool) {
		if exemptMarked && marked {
			return
		}
		if !pinned(window) {
			out = append(out, fmt.Sprintf("%s:%d", name, line))
		}
	})
	return out
}

// pinKeys are the two agent() pin axes validated against a literal allow-list.
var pinKeys = []string{"model", "effort"}

// pinAllowed is the allow-list per pin key; anything else is a hit (an unknown value silently falls
// back to the session tier rather than erroring).
var pinAllowed = map[string]map[string]bool{
	"model":  {"haiku": true, "sonnet": true, "opus": true, "fable": true},
	"effort": effortAllowed(),
}

// effortAllowed is the harness effort scale as a set.
func effortAllowed() map[string]bool {
	set := map[string]bool{}
	for _, e := range agentid.EffortNames() {
		set[e] = true
	}
	return set
}

var quotedLit = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`'([^']*)'|"([^"]*)"`) })

// badPinValues returns "name:line: ..." for each agent() pin literal that isn't in pinAllowed[key].
// Identifier-only values (s.model, a.implModel) pass — they can't be checked statically.
func badPinValues(name, src string) []string {
	var out []string
	scanAgentCalls(src, func(_, line int, window string, marked bool) {
		if marked {
			return
		}
		for _, key := range pinKeys {
			out = append(out, badPinValuesForKey(name, line, window, key)...)
		}
	})
	return out
}

// keyHit is one options-object key: at (its start, bare or quoted) and valStart (the byte after its colon).
type keyHit struct {
	at, valStart int
}

// topLevelKeyAt returns the bare or quoted `key:` hits sitting directly in the call's options object.
func topLevelKeyAt(src, key string) []keyHit {
	masked := stripStrings(src)
	var out []keyHit
	var open []int // offsets of the unclosed brackets, outermost first
	for i := 0; i < len(masked); i++ {
		switch c := masked[i]; c {
		case '(', '[', '{':
			open = append(open, i)
		case ')', ']', '}':
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		case '\'', '"', '`':
			if inOptionsObject(masked, open) && atKeyPosition(masked, i) {
				if vs, ok := quotedKeyValue(src, masked, i, key); ok {
					out = append(out, keyHit{i, vs})
				}
			}
			i = skipQuoted(masked, i, c)
			continue
		}
		if inOptionsObject(masked, open) && strings.HasPrefix(masked[i:], key+":") && (i == 0 || !isIdentByte(masked[i-1])) && atKeyPosition(masked, i) {
			out = append(out, keyHit{i, i + len(key) + 1})
		}
	}
	return out
}

// inOptionsObject reports whether the innermost bracket is the call's options `{`: directly inside the call's `(`,
// or inside grouping parens after a comma, as in agent('p', ({model:'sonnet'})).
func inOptionsObject(masked string, open []int) bool {
	n := len(open)
	if n < 2 || masked[open[n-1]] != '{' {
		return false
	}
	for k := 1; k < n-1; k++ {
		q := prevNonSpace(masked, open[k])
		if masked[open[k]] != '(' || q < 0 || (masked[q] != ',' && (k < 2 || q != open[k-1])) {
			return false
		}
	}
	return true
}

// prevNonSpace returns the index of the last non-whitespace byte before at, or -1.
func prevNonSpace(s string, at int) int {
	for j := at - 1; j >= 0; j-- {
		if !strings.ContainsRune(" \t\r\n", rune(s[j])) {
			return j
		}
	}
	return -1
}

// atKeyPosition reports whether the token at i follows the options object's `{` or a `,` (a key, not a value).
func atKeyPosition(masked string, i int) bool {
	j := prevNonSpace(masked, i) + 1
	return j > 0 && (masked[j-1] == '{' || masked[j-1] == ',')
}

// quotedKeyValue reports whether the literal at i is `'key'`/`"key"` plus a colon, returning the offset after it.
func quotedKeyValue(src, masked string, i int, key string) (int, bool) {
	end := skipQuoted(masked, i, masked[i])
	if end >= len(masked) || src[i+1:end] != key {
		return 0, false
	}
	j := skipSpaces(masked, end+1)
	if j < len(masked) && masked[j] == ':' {
		return j + 1, true
	}
	return 0, false
}

// hasTopLevelKey reports whether the call window pins key in its options object (bare or quoted).
func hasTopLevelKey(src, key string) bool { return len(topLevelKeyAt(src, key)) > 0 }

func badPinValuesForKey(name string, line int, window, key string) []string {
	var out []string
	masked := stripStrings(window)
	for _, hit := range topLevelKeyAt(window, key) {
		valStart := hit.valStart
		valEnd := len(masked)
		if k := strings.IndexAny(masked[valStart:], ",}\n"); k >= 0 {
			valEnd = valStart + k
		}
		for _, m := range quotedLit().FindAllStringSubmatch(window[valStart:valEnd], -1) {
			v := m[1]
			if v == "" {
				v = m[2]
			}
			if !pinAllowed[key][v] {
				out = append(out, fmt.Sprintf("%s:%d: agent() %s value %q is not one of %s"+
					" — an unknown value silently falls back to the session", name, line, key, v, allowedList(key)))
			}
		}
	}
	return out
}

// allowedList renders pinAllowed[key] as a deterministic, sorted "a|b|c" string for a message.
func allowedList(key string) string {
	keys := make([]string, 0, len(pinAllowed[key]))
	for k := range pinAllowed[key] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// scanAgentCalls calls fn once per agent( call in src — never subagent(/x.agent() — with its byte offset, 1-based
// line, its option window (through the matching close paren), and whether it carries the opus-ok
// marker on the line the call closes on.
func scanAgentCalls(src string, fn func(at, line int, window string, marked bool)) {
	masked := stripStrings(src) // same length as src: an `agent(` inside a string or comment is prose
	for i := 0; i+5 <= len(src); i++ {
		if masked[i:i+5] != "agent" {
			continue
		}
		if i > 0 && isIdentByte(src[i-1]) {
			continue // subagent(, myagent(, x.agent( — not the workflow primitive
		}
		if open := skipSpaces(masked, i+5); open >= len(masked) || masked[open] != '(' {
			continue // `agent (` with whitespace is the same call; `agentic(` is not
		}
		window := delimitedWindow(src, masked, i, '(', ')')
		markerEnd := i + len(window)
		if nl := strings.IndexByte(src[markerEnd:], '\n'); nl >= 0 {
			markerEnd += nl
		} else {
			markerEnd = len(src)
		}
		marked := strings.Contains(src[i:markerEnd], lintOpusOK)
		fn(i, 1+strings.Count(src[:i], "\n"), window, marked)
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// skipSpaces returns the index of the first non-whitespace byte at or after i (len(s) if none).
func skipSpaces(s string, i int) int {
	for i < len(s) && strings.ContainsRune(" \t\r\n", rune(s[i])) {
		i++
	}
	return i
}

// registryHidingDefects returns "file:line: <why>" for the two shapes that make the saved-workflow
// registry skip a script without any error: a `meta` that is not a pure literal (`+`, a template
// literal, or an identifier value), and a top-level `throw` — the module is imported to read meta, so it fires.
func registryHidingDefects(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("%s: cannot read file: %v", path, err)}
	}
	src := string(data)
	out := metaLiteralDefects(path, src)
	for _, line := range topLevelThrowLines(stripStrings(src)) {
		out = append(out, fmt.Sprintf("%s:%d: top-level throw — the registry imports the module to read meta, so it fires before any run; return {error} instead", path, line))
	}
	return out
}

var throwTokenRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bfunction\b|=>|\bthrow\b|\)\s*\{`)
})

// jsControlKeywords are the words that take a `(...) {` header but are not method names.
var jsControlKeywords = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "with": true}

// topLevelThrowLines scans module blocks but skips function, arrow and method-shorthand bodies.
func topLevelThrowLines(masked string) []int {
	var lines []int
	for at := 0; at < len(masked); {
		loc := throwTokenRe().FindStringIndex(masked[at:])
		if loc == nil {
			break
		}
		start, end := at+loc[0], at+loc[1]
		at = end
		tok := masked[start:end]
		switch {
		case strings.HasPrefix(tok, ")"):
			at = methodBodyEnd(masked, start, end)
		case tok == "=>":
			at = callableBodyEnd(masked, start, end)
		case start > 0 && isIdentByte(masked[start-1]):
			continue
		case tok == "throw":
			lines = append(lines, 1+strings.Count(masked[:start], "\n"))
		default: // function
			at = callableBodyEnd(masked, start, end)
		}
	}
	return lines
}

// methodBodyEnd returns the end of a method-shorthand body (`run(x) {`) whose `)` is at close, or end otherwise.
func methodBodyEnd(masked string, close, end int) int {
	open := matchingOpenParen(masked, close)
	if open < 0 || isControlHeader(masked, open) || !inMemberContainer(masked, open) {
		return end
	}
	return maskedGroupEnd(masked, end-1, '{', '}')
}

// memberHeadRe matches the text before a `{` that opens a class body or an expression-valued object literal.
var memberHeadRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:\bclass\b[^{};]*|[=(,\[?]|\breturn|\byield)\s*$`)
})

// inMemberContainer reports whether at sits directly inside a class body or an object literal.
func inMemberContainer(masked string, at int) bool {
	open := enclosingBrace(masked, at)
	return open >= 0 && isMemberBrace(masked, open)
}

// isMemberBrace reports whether the `{` at open opens a class body or object literal, not a block or label body.
func isMemberBrace(masked string, open int) bool {
	if memberHeadRe().MatchString(masked[:open]) {
		return true
	}
	colon := prevNonSpace(masked, open)
	return colon >= 0 && masked[colon] == ':' && !isLabelColon(masked, colon)
}

// isLabelColon reports whether the `:` at colon ends a statement label (`name:` opening a statement), not a key.
func isLabelColon(masked string, colon int) bool {
	e := prevNonSpace(masked, colon)
	if e < 0 || !isIdentByte(masked[e]) {
		return false
	}
	s := e
	for s > 0 && isIdentByte(masked[s-1]) {
		s--
	}
	p := prevNonSpace(masked, s)
	if p < 0 {
		return true
	}
	switch c := masked[p]; c {
	case ';', '}':
		return true
	case '{':
		return !isMemberBrace(masked, p)
	default: // a newline after an expression ends it, so the name starts a new statement
		return strings.Contains(masked[p+1:s], "\n") && (isIdentByte(c) || c == ')' || c == ']')
	}
}

// enclosingBrace returns the index of the `{` that opens the innermost block around at, or -1.
func enclosingBrace(masked string, at int) int {
	depth := 0
	for i := at - 1; i >= 0; i-- {
		switch masked[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// isControlHeader reports whether the `(` at open heads a control statement (`if (`, `for await (`), not a method.
func isControlHeader(masked string, open int) bool {
	word := identBefore(masked, open)
	if word != "await" {
		return jsControlKeywords[word]
	}
	before := strings.TrimSuffix(strings.TrimRight(masked[:open], " \t\r\n"), "await")
	return identBefore(before, len(before)) == "for"
}

// matchingOpenParen returns the index of the `(` that closes at close, or -1.
func matchingOpenParen(masked string, close int) int {
	depth := 0
	for i := close; i >= 0; i-- {
		switch masked[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// identBefore returns the identifier (dots included) that ends before at, skipping whitespace.
func identBefore(masked string, at int) string {
	j := prevNonSpace(masked, at) + 1
	k := j
	for k > 0 && isIdentByte(masked[k-1]) {
		k--
	}
	return masked[k:j]
}

// callableBodyEnd skips a function's parameters and block body, or an arrow's block body.
func callableBodyEnd(masked string, start, end int) int {
	if masked[start:end] == "function" {
		open := strings.IndexByte(masked[end:], '(')
		if open < 0 {
			return end
		}
		end = maskedGroupEnd(masked, end+open, '(', ')')
	}
	end = skipSpaces(masked, end)
	if end < len(masked) && masked[end] == '{' {
		return maskedGroupEnd(masked, end, '{', '}')
	}
	return end
}

// maskedGroupEnd returns the byte after a balanced group in string/comment-masked JavaScript.
func maskedGroupEnd(masked string, open int, opener, closer byte) int {
	depth := 0
	for i := open; i < len(masked); i++ {
		switch masked[i] {
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(masked)
}

// metaLiteralDefects flags an `export const meta` whose value is not a pure literal.
func metaLiteralDefects(path, src string) []string {
	start := strings.Index(src, "export const meta = {")
	if start < 0 {
		return nil
	}
	meta := delimitedWindow(src, stripStrings(src), start+len("export const meta = "), '{', '}')
	if !strings.Contains(meta, "`") && !strings.Contains(stripStrings(meta), "+") {
		return nil
	}
	return []string{fmt.Sprintf("%s:%d: meta is not a pure literal (no `+`, template literals or variables) — the registry cannot load it", path, 1+strings.Count(src[:start], "\n"))}
}

// delimitedWindow returns src from open through its matching closer, counting depth on masked (stripStrings(src)).
func delimitedWindow(src, masked string, open int, opener, closer byte) string {
	depth := 0
	i := open
	for ; i < len(masked) && i < open+pinWindow; i++ {
		switch masked[i] {
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return src[open : i+1]
			}
		}
	}
	return src[open:i]
}
