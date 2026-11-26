// Leaf test run — safety-scan's fifth rule: a leaf may not run any test (R37); the main session is not gated.

package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

const wholeTreeReason = "test runs are denied in a leaf (R37): build, vet and lint only; tests run once in the collector"

// wholeTreeMaxCmd is the longest command judged; a longer one is not a typed test run.
const wholeTreeMaxCmd = 8 << 10

// wholeTreeMaxParts is the most segments or lines judged; the shared cleaners are quadratic beyond it.
const wholeTreeMaxParts = 200

// wholeTreeRules is the one typed table: a command name and whether its arguments make it a test run.
var wholeTreeRules = map[string]func(args []string) bool{
	"go": goTest, "pytest": always, "python": pythonTest, "vitest": always, "jest": always,
	"npm": scriptRule("npm"), "pnpm": scriptRule("pnpm"), "yarn": scriptRule("yarn"), "bun": scriptRule("bun"), "npx": npxTest, "bunx": npxTest,
	"make": makeTest, "cargo": cargoTest,
}

// wholeTreeVerdict blocks a leaf's Bash command when a command in it is a test run; any doubt allows.
func wholeTreeVerdict(in *core.Input) (res core.Result) {
	defer func() {
		if recover() != nil {
			res = core.Result{}
		}
	}()
	cmd := in.ToolCommand()
	if isMainSession(in) || in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") || !wholeTreeCandidate(cmd) {
		return core.Result{}
	}
	view := dropAllHeredocBodies(intentView(dropMessageHeredocs(dropPatchBodies(cmd)), false))
	if !wholeTreeText(view, in.CWD, wholeTreeDepth) {
		return core.Result{}
	}
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", "leaf test run")
	return core.Result{Decision: core.Block, Floor: true, Rule: "leaf test run", Reason: wholeTreeReason}
}

// dropAllHeredocBodies drops every heredoc body: its text is data, never a command to judge.
func dropAllHeredocBodies(cmd string) string {
	return core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		ms := core.HeredocOp().FindAllStringSubmatch(lines[i], -1)
		if len(ms) != 1 {
			return -1
		}
		return core.HeredocEnd(lines, i+1, ms[0][2]+ms[0][3]+ms[0][4], ms[0][1] == "-")
	})
}

// wholeTreeDepth is how many nested `bash -c` or eval scripts are judged.
const wholeTreeDepth = 4

// substStart splits an unquoted $( or backtick substitution off as its own segment.
var substStart = strings.NewReplacer("$(", " ; ", "`", " ; ")

// wholeTreeText judges each segment of cmd, redirections dropped, and up to depth nested shell or eval scripts.
func wholeTreeText(cmd, cwd string, depth int) bool {
	if openQuote(cmd) {
		return false
	}
	hit := false
	shell.EachSegmentDir(substStart.Replace(cmd), cwd, func(seg string, _ []string, _ string, _ bool) {
		words, _ := shell.Redirects(seg)
		for _, start := range shell.CommandStarts(words) {
			hit = hit || wholeTreeStart(start)
			if inner, ok := shellString(start); ok && depth > 0 {
				hit = hit || wholeTreeText(dropAllHeredocBodies(intentView(inner, false)), cwd, depth-1)
			}
		}
	})
	for _, inner := range quotedSubsts(cmd) {
		hit = hit || depth > 0 && wholeTreeText(inner, cwd, depth-1)
	}
	return hit
}

// quotedSubsts is the body of each $(...) inside double quotes, where the segment parser sees one word.
func quotedSubsts(s string) []string {
	var out []string
	var q byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && q != '\'':
			i++
		case q == 0 && (c == '\'' || c == '"'):
			q = c
		case q == c:
			q = 0
		case q == '"' && c == '$' && i+1 < len(s) && s[i+1] == '(':
			end, depth := i+2, 1
			for ; end < len(s) && depth > 0; end++ {
				depth += strings.Count(s[end:end+1], "(") - strings.Count(s[end:end+1], ")")
			}
			if depth == 0 {
				out = append(out, s[i+2:end-1])
			}
		}
	}
	return out
}

// openQuote reports an unterminated quote: the parse is in doubt, so the command is allowed.
func openQuote(s string) bool {
	var q, prev byte
	ansi := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q == '\'' && ansi && c == '\\':
			i++
		case q == '\'' && c == '\'':
			q = 0
		case q == '"' && c == '\\':
			i++
		case q == '"' && c == '"':
			q = 0
		case q != 0:
		case c == '#' && (prev == 0 || prev == ' ' || prev == '\t' || prev == '\n' || prev == ';'):
			for i+1 < len(s) && s[i+1] != '\n' { // a comment's quotes open nothing
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			q, ansi = c, c == '\'' && prev == '$'
		}
		prev = c
	}
	return q != 0
}

// shellString is the script of `bash|sh|zsh -c '<script>' [words]` (later words are $0, $1...) or of `eval words`.
func shellString(start []string) (string, bool) {
	if len(start) > 1 && start[0] == "eval" {
		return strings.Join(start[1:], " "), true
	}
	if f := ""; len(start) > 1 {
		f = start[1]
		if len(start) < 3 || !strings.HasPrefix(f, "-") || strings.HasPrefix(f, "--") || !strings.HasSuffix(f, "c") {
			return "", false
		}
	}
	if len(start) < 3 || !slices.Contains([]string{"bash", "sh", "zsh"}, filepath.Base(start[0])) {
		return "", false
	}
	return start[2], true
}

// wholeTreeStart judges one command's words, past any NAME=value prefix.
func wholeTreeStart(start []string) bool {
	start = dropEnvAssigns(start)
	if len(start) == 0 {
		return false
	}
	name := filepath.Base(start[0])
	if v, ok := strings.CutPrefix(name, "python"); ok && strings.Trim(v, "0123456789.") == "" {
		name = "python" // python3.11 and the like
	}
	rule := wholeTreeRules[name]
	return rule != nil && rule(start[1:])
}

func always([]string) bool { return true }

// firstArg is the first argument that is no option, past options in valued (each skipped with its value).
func firstArg(args []string, valued []string, optionLike func(string) bool) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(valued, a):
			i++
		case optionLike(a):
		default:
			return i, true
		}
	}
	return 0, false
}

// goTest is `go test` past the global -C <dir> flag.
func goTest(args []string) bool {
	i, ok := firstArg(args, []string{"-C"}, func(a string) bool { return strings.HasPrefix(a, "-") })
	return ok && args[i] == "test"
}

// cargoTest is `cargo test`, past options and a +toolchain word.
func cargoTest(args []string) bool {
	i, ok := firstArg(args, nil, func(a string) bool { return strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+") })
	return ok && args[i] == "test"
}

// pythonTest is `python -m pytest`, past interpreter options (-W and -X take a value); -c ends the scan.
func pythonTest(args []string) bool {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-m":
			return i+1 < len(args) && (args[i+1] == "pytest" || args[i+1] == "unittest")
		case a == "-c", !strings.HasPrefix(a, "-"):
			return false
		case a == "-W" || a == "-X" || a == "--check-hash-based-pycs":
			i++
		}
	}
	return false
}

// scriptValued are the options that take a value for every package manager; npm also takes -w <name>.
var scriptValued = []string{"--prefix", "-C", "--dir", "--filter", "-F", "--cwd"}

// scriptRule is the argument rule of one package manager; for pnpm -w is boolean, for npm it takes a workspace name.
func scriptRule(tool string) func([]string) bool {
	valued := scriptValued
	if tool == "npm" {
		valued = append(slices.Clone(valued), "-w", "--workspace")
	}
	return func(args []string) bool { return scriptTest(args, valued) }
}

// scriptTest reads npm|pnpm|yarn|bun test, t, run test or test:*, and a direct jest or vitest.
func scriptTest(args, valued []string) bool {
	i, ok := firstArg(args, valued, func(a string) bool { return strings.HasPrefix(a, "-") })
	if !ok {
		return false
	}
	if args[i] == "run" || args[i] == "run-script" {
		j, ok := firstArg(args[i+1:], valued, func(a string) bool { return strings.HasPrefix(a, "-") })
		return ok && isTestScript(args[i+1+j])
	}
	if args[i] == "workspace" && i+2 < len(args) { // yarn workspace <name> <command>
		return scriptTest(args[i+2:], valued)
	}
	if slices.Contains([]string{"exec", "dlx", "x"}, args[i]) {
		return npxTest(args[i+1:])
	}
	return slices.Contains([]string{"test", "t", "jest", "vitest"}, args[i])
}

func isTestScript(n string) bool { return n == "test" || strings.HasPrefix(n, "test:") }

// npxTest is npx|bunx vitest|jest, past the options before the package name (-p and --package take a value).
func npxTest(args []string) bool {
	i, ok := firstArg(args, []string{"-p", "--package"}, func(a string) bool { return strings.HasPrefix(a, "-") })
	return ok && (args[i] == "vitest" || args[i] == "jest" || args[i] == "pytest")
}

var makeTargets = []string{"test", "audit", "verify"}

// makeValued are the make options that take a value.
var makeValued = []string{"-f", "--file", "--makefile", "-C", "--directory", "-I", "--include-dir", "-o", "--old-file",
	"--assume-old", "-W", "--what-if", "--new-file", "--assume-new"}

// makeTest is make with a test, audit or verify target; options and VAR=value words are skipped.
func makeTest(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(makeValued, a):
			i++
		case a == "-j" || a == "--jobs":
			if i+1 < len(args) && isDigits(args[i+1]) {
				i++
			}
		case strings.HasPrefix(a, "-"), strings.Contains(a, "="):
		case slices.Contains(makeTargets, a):
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// wholeTreeCandidate is a cheap pre-check: the command names a test runner and is small enough to judge.
func wholeTreeCandidate(cmd string) bool {
	if len(cmd) > wholeTreeMaxCmd || strings.Count(cmd, "\n")+strings.Count(cmd, "|")+strings.Count(cmd, ";")+
		strings.Count(cmd, "&") > wholeTreeMaxParts {
		return false
	}
	for _, w := range []string{"test", "jest", "make", "npm", "yarn", "bun"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}
