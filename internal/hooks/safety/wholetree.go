// Whole-tree test run — safety-scan's fifth rule: a leaf may not run the suite (R37); the main session is not gated.

package safety

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

const wholeTreeReason = "whole-tree test run denied in a leaf (R37): run only the tests you wrote or repair, by exact " +
	"name: go test -run '^TestX$' ./internal/<pkg>; the suite runs once in the collector"

// wholeTreeMaxCmd is the longest command judged; a longer one is not a typed test run.
const wholeTreeMaxCmd = 8 << 10

// wholeTreeMaxParts is the most segments or lines judged; the shared cleaners are quadratic beyond it.
const wholeTreeMaxParts = 200

// wholeTreeRules is the one typed table: a command name and whether its arguments are exactly a suite-run shape.
var wholeTreeRules = map[string]func(args []string) bool{
	"go": goSuite, "pytest": pytestSuite, "python": pythonSuite, "python3": pythonSuite,
	"npm": scriptSuite, "pnpm": scriptSuite, "yarn": scriptSuite, "bun": scriptSuite,
	"npx": npxSuite, "vitest": jsSuite, "jest": jsSuite, "make": makeSuite,
}

// wholeTreeVerdict blocks a leaf's Bash command when a command in it is exactly a suite run; any doubt allows.
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
	if !wholeTreeText(view, in.CWD, true) {
		return core.Result{}
	}
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", "whole-tree test")
	return core.Result{Decision: core.Block, Floor: true, Rule: "whole-tree test", Reason: wholeTreeReason}
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

// wholeTreeText judges each segment of cmd, redirections dropped, and with nest the string of a `bash|sh|zsh -c`.
func wholeTreeText(cmd, cwd string, nest bool) bool {
	hit := false
	shell.EachSegmentDir(cmd, cwd, func(seg string, _ []string, _ string, _ bool) {
		words, _ := shell.Redirects(seg)
		for _, start := range shell.CommandStarts(words) {
			hit = hit || wholeTreeStart(start)
			if inner, ok := shellString(start); ok && nest {
				hit = hit || wholeTreeText(dropAllHeredocBodies(intentView(inner, false)), cwd, false)
			}
		}
	})
	return hit
}

// shellString is the string after -c of a bare `bash|sh|zsh -c '<string>'` command.
func shellString(start []string) (string, bool) {
	if len(start) != 3 || start[1] != "-c" || !slices.Contains([]string{"bash", "sh", "zsh"}, filepath.Base(start[0])) {
		return "", false
	}
	return start[2], true
}

// wholeTreeStart judges one command's words, past any NAME=value prefix.
func wholeTreeStart(start []string) bool {
	for len(start) > 0 && isEnvAssign(start[0]) {
		start = start[1:]
	}
	if len(start) == 0 {
		return false
	}
	rule := wholeTreeRules[filepath.Base(start[0])]
	return rule != nil && rule(start[1:])
}

func isEnvAssign(t string) bool {
	i := strings.IndexByte(t, '=')
	return i > 0 && !strings.ContainsAny(t[:i], "-/.")
}

// only reports that every word is in the allowed set (an empty list counts).
func only(args []string, allowed ...string) bool {
	return !slices.ContainsFunc(args, func(a string) bool { return !slices.Contains(allowed, a) })
}

var goBoolFlags = []string{"-v", "-race", "-short", "-json", "-cover", "-failfast", "-x", "-count=1"}

// goSuite is `go test` with a literal ./..., ... or all package and no flag that narrows or hands off the run.
func goSuite(args []string) bool {
	if len(args) == 0 || args[0] != "test" {
		return false
	}
	if slices.ContainsFunc(args[1:], goNarrows) {
		return false
	}
	return goTreeArg(args)
}

// goNarrows reports a flag (`-run`, `--run`, `-test.run=x`) that narrows the run or hands the line to the binary.
func goNarrows(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	if len(name) > 2 && strings.HasPrefix(name, "--") {
		name = name[1:]
	}
	return slices.Contains(goNarrowing, name)
}

// goTreeArg reports a tree pattern among args that is a package, not the value of the flag before it.
func goTreeArg(args []string) bool {
	for i := 1; i < len(args); i++ {
		prev := args[i-1]
		flagValue := i > 1 && strings.HasPrefix(prev, "-") && !strings.Contains(prev, "=") && !slices.Contains(goBoolFlags, prev)
		if (args[i] == "..." || args[i] == "all" || strings.HasSuffix(args[i], "/...")) && !flagValue {
			return true
		}
	}
	return false
}

// goNarrowing are the flags that select tests, list, compile only or hand the rest of the line to the binary.
var goNarrowing = []string{"-run", "-bench", "-list", "-c", "-C", "-args", "--", "-fuzz", "-test.run", "-test.bench",
	"-test.list", "-test.fuzz"}

// wholeTreeCandidate is a cheap pre-check: the command names a test runner and is small enough to judge.
func wholeTreeCandidate(cmd string) bool {
	if len(cmd) > wholeTreeMaxCmd || strings.Count(cmd, "\n")+strings.Count(cmd, "|")+strings.Count(cmd, ";")+
		strings.Count(cmd, "&") > wholeTreeMaxParts {
		return false
	}
	for _, w := range []string{"go test", "pytest", "vitest", "jest", " test", "make "} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}

func pytestSuite(args []string) bool {
	return only(args, "-q", "-v", "-vv", "-x", "-s", ".", "src", "tests", "test")
}

func pythonSuite(args []string) bool {
	return len(args) > 1 && args[0] == "-m" && args[1] == "pytest" && pytestSuite(args[2:])
}

func jsSuite(args []string) bool { return only(args, "run", "src", "--run") }

// scriptSuite reads npm|pnpm|yarn|bun test and run test.
func scriptSuite(args []string) bool {
	if len(args) > 1 && args[0] == "run" {
		args = args[1:]
	}
	return len(args) > 0 && args[0] == "test" && jsSuite(args[1:])
}

func npxSuite(args []string) bool {
	return len(args) > 0 && (args[0] == "vitest" || args[0] == "jest") && jsSuite(args[1:])
}

func makeSuite(args []string) bool {
	return len(args) == 1 && slices.Contains([]string{"test", "audit", "verify"}, args[0])
}
