package plan

// `workflow lint` — flag workflow agent() calls without an explicit model/agentType pin or an effort
// pin (an unpinned leaf inherits the parent session's reasoning tier, a measured cost trap), and the
// two script shapes that make the saved-workflow registry skip a file silently.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
			fmt.Fprintln(stdout, "usage: claude-code workflow lint [<root>]   (lints <root>/workflows/*.js)")
			return 0
		}
		root = args[0]
	}
	files, _ := filepath.Glob(filepath.Join(root, "workflows", "*.js"))
	bad := 0
	for _, f := range files {
		for _, hit := range unpinnedAgentCalls(f) {
			fmt.Fprintf(stderr, "%s: agent() without model:/agentType: — the leaf inherits the session reasoning tier; pin model:'sonnet', use agentType:'code-workers', or mark // %s\n", hit, lintOpusOK)
			bad++
		}
		for _, hit := range effortlessAgentCalls(f) {
			fmt.Fprintf(stderr, "%s: agent() without effort: — pin effort:'low' for mechanical leaves, 'medium' for standard, or mark // %s\n", hit, lintOpusOK)
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

// unpinnedAgentCalls returns "file:line" for each agent( call whose option window carries neither a
// model/agentType pin nor the opus-ok marker.
func unpinnedAgentCalls(path string) []string {
	return agentCallsMissing(path, func(window string) bool {
		return strings.Contains(window, "model:") || strings.Contains(window, "agentType:")
	})
}

// effortlessAgentCalls returns "file:line" for each agent( call with no effort: pin — effort is the
// second routing axis (rules/workflow/agents.md); unpinned runs at session effort, over-costing.
func effortlessAgentCalls(path string) []string {
	return agentCallsMissing(path, func(window string) bool {
		return strings.Contains(window, "effort:")
	})
}

// agentCallsMissing returns "file:line" for each agent( call whose option window fails `pinned` and
// carries no opus-ok marker. String heuristic, not a JS parser; an unreadable file is a hit (fail closed).
func agentCallsMissing(path string, pinned func(window string) bool) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("%s: cannot read file: %v", path, err)}
	}
	src := string(data)
	var out []string
	for i := 0; i+6 <= len(src); i++ {
		if src[i:i+6] != "agent(" {
			continue
		}
		if i > 0 && isIdentByte(src[i-1]) {
			continue // subagent(, myagent(, x.agent( — not the workflow primitive
		}
		window := callWindow(src, i)
		// The opus-ok marker rides the line the call CLOSES on (after the options object).
		markerEnd := i + len(window)
		if nl := strings.IndexByte(src[markerEnd:], '\n'); nl >= 0 {
			markerEnd += nl
		} else {
			markerEnd = len(src)
		}
		// Strip string-literal content so a prompt mentioning "model:"/"effort:" can't fake a pin.
		if pinned(stripStrings(window)) || strings.Contains(src[i:markerEnd], lintOpusOK) {
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", path, 1+strings.Count(src[:i], "\n")))
	}
	return out
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
	for i, l := range strings.Split(src, "\n") {
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
				b[k] = ' '
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
			b[i] = ' '
		}
	}
	return string(b)
}
