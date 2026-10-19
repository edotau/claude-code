package plan

// `workflow lint` — flag workflow agent() calls without an explicit model/agentType pin or an effort
// pin (an unpinned leaf inherits the parent session's reasoning tier, a measured cost trap), a pin
// whose literal value isn't a real tier, and the two script shapes that make the saved-workflow
// registry skip a file silently. The `lint:opus-ok` marker exempts only the effort check — a missing
// model:/agentType: pin is always a hit.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
)

// lintOpusOK marks a deliberate reasoning-tier agent() call (on the line the call closes on).
const lintOpusOK = "lint:opus-ok"

// pinWindow caps the scan when a call's close paren can't be found (unbalanced string content).
// Generous: prompt template literals routinely run into the thousands of characters.
const pinWindow = 20000

// RunWorkflowLint lints every workflows/*.js under root (default: the config dir the caller passes as
// args[0]; a bare call uses cwd). Exit 1 when anything is flagged.
func RunWorkflowLint(args []string, stdout, stderr io.Writer) int {
	root := "."
	if len(args) > 0 && args[0] != "" {
		if args[0] == "-h" || args[0] == "--help" {
			helptext.Fprint(stdout, "usage: claude-code workflow lint [<root>]   (lints <root>/workflows/*.js)\n")
			return 0
		}
		root = args[0]
	}
	files, _ := filepath.Glob(filepath.Join(root, "workflows", "*.js"))
	bad := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "%s: cannot read file: %v\n", f, err)
			bad++
			continue
		}
		for _, hit := range LintPins(f, data) {
			fmt.Fprintln(stderr, hit)
			bad++
		}
		for _, hit := range registryHidingDefects(f) {
			fmt.Fprintln(stderr, hit)
			bad++
		}
	}
	if bad > 0 {
		return 1
	}
	fmt.Fprintf(stdout, "workflow lint: %d script(s) clean\n", len(files))
	return 0
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
	return agentCallsMissingSrc(name, src, func(window string) bool { return strings.Contains(window, "model:") }, false)
}

// effortlessAgentCalls returns "name:line" for each agent( call with no effort: pin — effort is the
// second routing axis (rules/workflow/agents.md); unpinned runs at session effort, over-costing.
func effortlessAgentCalls(name, src string) []string {
	return agentCallsMissingSrc(name, src, func(window string) bool {
		return strings.Contains(window, "effort:")
	}, true)
}

// agentCallsMissingSrc returns "name:line" for each agent() call whose (string-stripped) option
// window fails `pinned`. exemptMarked honors the lint:opus-ok marker; pass false for a check D1 says
// the marker must never waive (the model/agentType pin).
func agentCallsMissingSrc(name, src string, pinned func(window string) bool, exemptMarked bool) []string {
	var out []string
	scanAgentCalls(src, func(line int, window string, marked bool) {
		if exemptMarked && marked {
			return
		}
		if !pinned(stripStrings(window)) {
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
	"effort": {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
}

var quotedLit = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// badPinValues returns "name:line: ..." for each agent() pin literal that isn't in pinAllowed[key].
// Identifier-only values (s.model, a.implModel) pass — they can't be checked statically.
func badPinValues(name, src string) []string {
	var out []string
	scanAgentCalls(src, func(line int, window string, marked bool) {
		if marked {
			return
		}
		s := stripStrings(window)
		for _, key := range pinKeys {
			needle := key + ":"
			for idx := 0; ; {
				j := strings.Index(s[idx:], needle)
				if j < 0 {
					break
				}
				pos := idx + j
				idx = pos + len(needle)
				if pos > 0 && isIdentByte(s[pos-1]) {
					continue
				}
				valStart := pos + len(needle)
				valEnd := len(s)
				if k := strings.IndexAny(s[valStart:], ",}\n"); k >= 0 {
					valEnd = valStart + k
				}
				for _, m := range quotedLit.FindAllStringSubmatch(window[valStart:valEnd], -1) {
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
		}
	})
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

// scanAgentCalls calls fn once per agent( call in src — never subagent(/x.agent() — with its 1-based
// line, its option window (through the matching close paren), and whether it carries the opus-ok
// marker on the line the call closes on.
func scanAgentCalls(src string, fn func(line int, window string, marked bool)) {
	masked := stripStrings(src) // same length as src: an `agent(` inside a string or comment is prose
	for i := 0; i+6 <= len(src); i++ {
		if masked[i:i+6] != "agent(" {
			continue
		}
		if i > 0 && isIdentByte(src[i-1]) {
			continue // subagent(, myagent(, x.agent( — not the workflow primitive
		}
		window := callWindow(src, i)
		markerEnd := i + len(window)
		if nl := strings.IndexByte(src[markerEnd:], '\n'); nl >= 0 {
			markerEnd += nl
		} else {
			markerEnd = len(src)
		}
		marked := strings.Contains(src[i:markerEnd], lintOpusOK)
		fn(1+strings.Count(src[:i], "\n"), window, marked)
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
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
	var out []string
	line := func(i int) int { return 1 + strings.Count(src[:i], "\n") }
	if start := strings.Index(src, "export const meta = {"); start >= 0 {
		meta := delimitedWindow(src, start+len("export const meta = "), '{', '}')
		if strings.Contains(meta, "`") || strings.Contains(stripStrings(meta), "+") {
			out = append(out, fmt.Sprintf("%s:%d: meta is not a pure literal (no `+`, template literals or variables) — the registry cannot load it", path, line(start)))
		}
	}
	for i, l := range strings.Split(stripStrings(src), "\n") { // masked: a prompt line starting "throw " is prose
		t := strings.TrimSpace(l)
		if l != "" && l[0] != ' ' && l[0] != '\t' && (strings.HasPrefix(t, "throw ") || (strings.HasPrefix(t, "if ") && strings.Contains(t, ") throw "))) {
			out = append(out, fmt.Sprintf("%s:%d: top-level throw — the registry imports the module to read meta, so it fires before any run; return {error} instead", path, i+1))
		}
	}
	return out
}

// callWindow returns src from the `agent(` at start through its matching close paren.
func callWindow(src string, start int) string {
	return delimitedWindow(src, start+len("agent(")-1, '(', ')')
}

// delimitedWindow returns src from the opener at open through its matching closer, skipping string
// literals (backtick/single/double, with \-escapes) so prompt text can't skew the depth. Runs out at
// pinWindow or end of input when unbalanced.
func delimitedWindow(src string, open int, opener, closer byte) string {
	depth := 0
	i := open
	for ; i < len(src) && i < open+pinWindow; i++ {
		switch c := src[i]; c {
		case '`', '\'', '"':
			for i++; i < len(src); i++ {
				if src[i] == '\\' {
					i++
					continue
				}
				if src[i] == c {
					break
				}
			}
		case opener:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return src[open-countBack(src, open) : i+1]
			}
		}
	}
	if i > len(src) {
		i = len(src)
	}
	return src[open-countBack(src, open) : i]
}

// countBack is the length of the `agent` word preceding a call's opening paren, so a call window starts
// at the identifier; a meta window starts at its brace.
func countBack(src string, open int) int {
	if strings.HasSuffix(src[:open], "agent") {
		return len("agent")
	}
	return 0
}

// stripStrings blanks '…', "…" and `…` literal contents so prompt prose is never read as a pin or operator.
func stripStrings(src string) string {
	b := []byte(src)
	for i := 0; i < len(b); i++ {
		c := b[i]
		// A comment's apostrophe ("don't") must not open a string and blank the real pin after it.
		if c == '/' && i+1 < len(b) && (b[i+1] == '/' || b[i+1] == '*') {
			end := "\n"
			if b[i+1] == '*' {
				end = "*/"
			}
			j := strings.Index(string(b[i+2:]), end)
			if j < 0 {
				j = len(b) - i - 2
			}
			for k := i; k < i+2+j && k < len(b); k++ {
				if b[k] != '\n' { // keep line structure so callers can still count lines
					b[k] = ' '
				}
			}
			i += 1 + j
			continue
		}
		if c != '\'' && c != '"' && c != '`' {
			continue
		}
		for i++; i < len(b) && b[i] != c; i++ {
			if b[i] == '\\' {
				i++
				continue
			}
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// argSpan is one top-level call argument, as a [start,end) byte range relative to the args substring.
type argSpan struct {
	start, end int
}

// splitTopLevelArgs splits a call's argument-list text at top-level commas, skipping string/template
// literals (with \-escapes) and ()[]{} nesting, so a comma inside a prompt or object never splits.
func splitTopLevelArgs(s string) []argSpan {
	var spans []argSpan
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '`', '\'', '"':
			for i++; i < len(s); i++ {
				if s[i] == '\\' {
					i++
					continue
				}
				if s[i] == c {
					break
				}
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				spans = append(spans, argSpan{start, i})
				start = i + 1
			}
		}
	}
	return append(spans, argSpan{start, len(s)})
}

// FillModel rewrites every agent() call in src missing a model:/agentType: pin (opus-ok does NOT
// exempt this) to carry model: '<model>', inserted into an existing options-object 2nd argument, or
// appended as a new one. Edits apply back-to-front so offsets stay valid; a call whose options
// argument isn't a plain object literal (identifier, spread, ternary, call, or an unbalanced window)
// is left alone and its line reported in ambiguous instead. src is returned unchanged if nothing
// needed filling.
func FillModel(src, model string) (string, []string) {
	type edit struct {
		at   int
		text string
	}
	var edits []edit
	var ambiguous []string
	masked := stripStrings(src)
	for i := 0; i+6 <= len(src); i++ {
		if masked[i:i+6] != "agent(" {
			continue
		}
		if i > 0 && isIdentByte(src[i-1]) {
			continue
		}
		window := callWindow(src, i)
		stripped := stripStrings(window)
		if strings.Contains(stripped, "model:") {
			continue // already pinned (agentType alone is not a pin — Explore runs opus)
		}
		line := 1 + strings.Count(src[:i], "\n")
		if len(window) < 6 || window[len(window)-1] != ')' {
			ambiguous = append(ambiguous, fmt.Sprintf("line %d", line))
			continue
		}
		argsStart := i + len("agent(")
		argsEnd := i + len(window) - 1
		argsSrc := src[argsStart:argsEnd]
		spans := splitTopLevelArgs(argsSrc)
		if len(spans) == 1 && strings.TrimSpace(argsSrc) == "" {
			ambiguous = append(ambiguous, fmt.Sprintf("line %d", line))
			continue
		}
		switch len(spans) {
		case 1:
			edits = append(edits, edit{argsEnd, fmt.Sprintf(", { model: '%s' }", model)})
		case 2:
			opt := argsSrc[spans[1].start:spans[1].end]
			trimmed := strings.TrimLeft(opt, " \t\n")
			if trimmed == "" || trimmed[0] != '{' {
				ambiguous = append(ambiguous, fmt.Sprintf("line %d", line))
				continue
			}
			braceAt := argsStart + spans[1].start + (len(opt) - len(trimmed))
			rest := strings.TrimLeft(src[braceAt+1:argsEnd], " \t\n")
			if strings.HasPrefix(rest, "}") {
				edits = append(edits, edit{braceAt + 1, fmt.Sprintf("model: '%s'", model)})
			} else {
				edits = append(edits, edit{braceAt + 1, fmt.Sprintf(" model: '%s',", model)})
			}
		default:
			ambiguous = append(ambiguous, fmt.Sprintf("line %d", line))
		}
	}
	if len(edits) == 0 {
		return src, ambiguous
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].at > edits[b].at })
	out := src
	for _, e := range edits {
		out = out[:e.at] + e.text + out[e.at:]
	}
	return out, ambiguous
}
