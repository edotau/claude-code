// Whole-suite test run — safety-scan's fifth rule: the suite runs once, on n=0, by the main session.

package safety

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/audit"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

const suiteWhy = "whole-suite test run denied: the suite runs once, on the local working branch, by the main session"

// suiteMaxCmd bounds the raw text and suiteMaxParts its heredoc-free view; the shared cleaners are quadratic beyond.
const (
	suiteMaxCmd   = 64 << 10
	suiteMaxParts = 200
	suiteMaxNest  = 4
)

// suiteWords are the cheap pre-check: a command holding none of them names no test runner the table knows.
var suiteWords = []string{"test", "jest", "tox", "audit", "verify", "-run", "build", "vet", "diff"}

// suiteRule is one runner: whether its arguments are a whole-suite run, and the narrowed form to suggest.
type suiteRule struct {
	suite func(args []string) bool
	form  string
}

const (
	goForm      = "go test -run '^TestName$' ./internal/<pkg>"
	goBuildForm = "go build ./internal/<pkg> && go vet ./internal/<pkg>"
	diffForm    = "claude-code agents diff --check --gates lint,complexity --path <files>"
	pyForm      = "pytest path/test_x.py::test_name"
	jsForm      = "npx vitest run src/x.test.ts -t name"
	mvnForm     = "mvn test -Dtest=ClassTest#method"
	gradForm    = "gradle test --tests 'ClassTest.method'"
)

// suiteRules is the one typed table, keyed by command basename.
var suiteRules = map[string]suiteRule{
	"go": {goSuite, goForm}, "make": {makeSuite, goForm + " (one package, by test name)"},
	"pytest": {pytestSuite, pyForm}, "py.test": {pytestSuite, pyForm}, "tox": {toxSuite, "tox -- " + pyForm},
	"python": {pythonSuite, pyForm}, "python3": {pythonSuite, pyForm},
	"npm": {scriptSuite, jsForm}, "pnpm": {scriptSuite, jsForm}, "yarn": {scriptSuite, jsForm}, "bun": {scriptSuite, jsForm},
	"npx": {npxSuite, jsForm}, "bunx": {npxSuite, jsForm}, "vitest": {jsSuite, jsForm}, "jest": {jsSuite, jsForm},
	"cargo": {cargoSuite, "cargo test <test_name>"}, "mvn": {mvnSuite, mvnForm}, "mvnw": {mvnSuite, mvnForm},
	"gradle": {gradleSuite, gradForm}, "gradlew": {gradleSuite, gradForm},
}

// suiteVerdict blocks a whole-suite run by an agent, or by anyone in a linked worktree; any doubt allows.
func suiteVerdict(in *core.Input) (res core.Result) {
	defer func() {
		if recover() != nil {
			res = core.Result{}
		}
	}()
	cmd := in.ToolCommand()
	if in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") || !suiteCandidate(cmd) {
		return core.Result{}
	}
	view, ok := suiteView(dropMessageHeredocs(dropPatchBodies(cmd)))
	if !ok {
		return core.Result{}
	}
	form := suiteText(view, in.CWD, !isMainSession(in), false, suiteMaxNest)
	if form == "" {
		return core.Result{}
	}
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", "whole-suite test")
	return core.Result{Decision: core.Block, Floor: true, Rule: "whole-suite test",
		Reason: suiteWhy + ".\nRun only the tests of the files you touch, by name: " + form}
}

// suiteCandidate is the early exit: small enough to judge and naming a test-runner word.
func suiteCandidate(cmd string) bool {
	return len(cmd) <= suiteMaxCmd && slices.ContainsFunc(suiteWords, func(w string) bool { return strings.Contains(cmd, w) })
}

// suiteView drops heredoc bodies, comments and data words (echo, grep, commit text); ok=false when unsure.
func suiteView(cmd string) (string, bool) {
	cmd, ok := dropHeredocs(cmd)
	if !ok || strings.Count(cmd, "\n")+strings.Count(cmd, "|")+strings.Count(cmd, ";")+
		strings.Count(cmd, "&") > suiteMaxParts {
		return "", false
	}
	return intentView(shell.DropComments(cmd), false), true
}

// dropHeredocs drops every heredoc body and its closing line; ok=false when one cannot be read.
func dropHeredocs(cmd string) (string, bool) {
	if !strings.Contains(cmd, "<<") {
		return cmd, true
	}
	lines := strings.Split(cmd, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		if !strings.Contains(lines[i], "<<") || strings.Contains(lines[i], "<<<") {
			continue
		}
		ms := core.HeredocOp().FindAllStringSubmatch(lines[i], -1)
		if len(ms) > 1 {
			return cmd, false
		}
		if len(ms) == 1 {
			end := core.HeredocEnd(lines, i+1, ms[0][2]+ms[0][3]+ms[0][4], ms[0][1] == "-")
			if end < 0 {
				return cmd, false
			}
			i = end
		}
	}
	return strings.Join(out, "\n"), true
}

// suiteText is the allowed form for the first denied whole-suite run in cmd ("" when none); nest bounds bash -c.
func suiteText(cmd, cwd string, agent, lost bool, nest int) string {
	form := ""
	shell.EachSegmentDir(cmd, cwd, func(seg string, _ []string, dir string, segLost bool) {
		words, _ := shell.Redirects(seg)
		_, dir = shell.Unwrap(shell.DropRtk(words), dir) // env -C and sudo -D move the command
		lost := lost || segLost
		judged := ""
		for _, start := range shell.CommandStarts(words) {
			if form != "" {
				return
			}
			if f, at := suiteStart(start, dir, agent); f != "" && (agent || !lost && linkedWorktree(at)) {
				form = f
			} else if inner, ok := shell.NestedCommand(start); ok && nest > 0 && inner != judged {
				judged = inner // a wrapped start and its unwrapped twin name one inner command
				if view, ok := suiteView(inner); ok {
					form = suiteText(view, dir, agent, lost, nest-1)
				}
			}
		}
	})
	return form
}

// linkedWorktree reports dir inside a linked git worktree: its .git file points at a gitdir holding commondir.
func linkedWorktree(dir string) bool {
	if dir == "" {
		return false
	}
	top, _ := paths.CheckoutOf(paths.Real(dir))
	if top == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(top, ".git"))
	ptr, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if err != nil || !ok {
		return false
	}
	if ptr = strings.TrimSpace(ptr); !filepath.IsAbs(ptr) {
		ptr = filepath.Join(top, ptr)
	}
	_, err = os.Stat(filepath.Join(ptr, "commondir"))
	return err == nil
}

// suiteStart is the allowed form when start is a whole-suite run, past NAME=value prefixes, and the dir it runs in.
func suiteStart(start []string, dir string, agent bool) (form, at string) {
	for len(start) > 0 && isEnvAssign(start[0]) {
		start = start[1:]
	}
	if len(start) == 0 {
		return "", dir
	}
	if f, gateDir := harnessSuite(shell.HarnessArgs(start), agent); f != "" {
		return f, movedDir(dir, gateDir)
	}
	return runnerSuite(filepath.Base(start[0]), start[1:], dir, agent)
}

func runnerSuite(name string, args []string, dir string, agent bool) (string, string) {
	if agent && name == "go" && goAgentBuildVet(args) {
		return goBuildForm, dir
	}
	rule, ok := suiteRules[name]
	if !ok || !rule.suite(args) {
		return "", dir
	}
	if name == "make" {
		dir = makeDir(args, dir)
	}
	return rule.form, dir
}

func isEnvAssign(t string) bool {
	i := strings.IndexByte(t, '=')
	return i > 0 && !strings.ContainsAny(t[:i], "-/.")
}

// movedDir is dir after a chdir to to: to itself when absolute, joined when relative, "" (unknown) when dynamic.
func movedDir(dir, to string) string {
	switch {
	case to == "":
		return dir
	case strings.ContainsAny(to, "$`~"):
		return ""
	case filepath.IsAbs(to):
		return to
	case dir == "":
		return ""
	}
	return filepath.Join(dir, to)
}

// makeDir applies each `make -C dir` / `--directory dir` in order, as make does.
func makeDir(args []string, dir string) string {
	for i := 0; i < len(args); i++ {
		a, to := args[i], ""
		switch {
		case (a == "-C" || a == "--directory") && i+1 < len(args):
			to, i = args[i+1], i+1
		case strings.HasPrefix(a, "--directory="):
			to = a[len("--directory="):]
		case strings.HasPrefix(a, "-C"):
			to = a[2:]
		}
		dir = movedDir(dir, to)
	}
	return dir
}

// harnessSuite checks worktree gate/land and agents diff --check whole-suite forms, returning form and --dir.
func harnessSuite(args []string, agent bool) (string, string) {
	rest, ok := harnessRest(args)
	if !ok {
		return "", ""
	}
	const wtForm = "claude-code worktree gate --run --scope <dirs you changed>, or worktree land without --full-verify"
	switch args[0] + " " + args[1] {
	case "worktree gate":
		if boolFlag(rest, "run") && strings.Trim(flagValue(rest, "scope"), " ,") == "" {
			return wtForm, flagValue(rest, "dir")
		}
	case "worktree land":
		if boolFlag(rest, "full-verify") {
			return wtForm, flagValue(rest, "dir")
		}
	case "agents diff":
		if diffSuite(rest, agent) {
			return diffForm, flagValue(rest, "dir")
		}
	}
	return "", ""
}

func harnessRest(args []string) ([]string, bool) {
	if len(args) < 2 {
		return nil, false
	}
	rest := args[2:]
	if i := slices.Index(rest, "--"); i >= 0 {
		rest = rest[:i]
	}
	return rest, !boolFlag(rest, "help") && !boolFlag(rest, "h")
}

// diffSuite matches `agents diff --check --all` without `--path`, or an agent `diff --check` that runs package tests.
func diffSuite(rest []string, agent bool) bool {
	if !boolFlag(rest, "check") {
		return false
	}
	if boolFlag(rest, "all") && strings.Trim(flagValue(rest, "path"), " ,") == "" {
		return true
	}
	gates := strings.Trim(flagValue(rest, "gates"), " ,")
	return agent && (gates == "" || gates == "all" || slices.Contains(strings.Split(gates, ","), "test"))
}

// goAgentBuildVet matches `go build ./...` or `go vet ./...` on a package tree when run by a subagent.
func goAgentBuildVet(args []string) bool {
	if len(args) == 0 || args[0] != "build" && args[0] != "vet" || slices.Contains(args[1:], "-n") {
		return false
	}
	return slices.ContainsFunc(args[1:], func(a string) bool { return !strings.HasPrefix(a, "-") && goTreeArg(a) })
}

// boolFlag is the last setting of a Go bool flag (`-name`, `--name`, `--name=<bool>`); a bad value counts false.
func boolFlag(args []string, name string) bool {
	on := false
	for _, a := range args {
		flag, val, set := strings.Cut(strings.TrimPrefix(a, "-"), "=")
		if flag != "-"+name && flag != name {
			continue
		}
		b, err := strconv.ParseBool(val)
		on = !set || err == nil && b
	}
	return on
}

// flagValue is the last value of a Go string flag (`-name v`, `--name=v`), "" when absent or empty.
func flagValue(args []string, name string) string {
	v := ""
	for i, a := range args {
		flag, val, set := strings.Cut(strings.TrimPrefix(a, "-"), "=")
		switch {
		case flag != "-"+name && flag != name:
		case set:
			v = val
		case i+1 < len(args):
			v = args[i+1]
		}
	}
	return v
}

// goNarrowing select tests, print or compile only, move the directory or hand the rest of the line on.
var goNarrowing = []string{"-run", "-bench", "-list", "-fuzz", "-n", "-c", "-C", "--", "-test.run", "-test.bench",
	"-test.list", "-test.fuzz"}

// goValueFlags take the next word as their value, so that word is no package.
var goValueFlags = []string{"-p", "-o", "-tags", "-coverpkg", "-covermode", "-coverprofile", "-timeout", "-count",
	"-cpu", "-parallel", "-exec", "-mod", "-modfile", "-gcflags", "-ldflags", "-asmflags", "-vet", "-shuffle",
	"-outputdir", "-cpuprofile", "-memprofile", "-blockprofile", "-mutexprofile", "-trace", "-benchtime", "-pkgdir",
	"-overlay", "-toolexec", "-installsuffix", "-skip"}

// goSuite is `go test` with a tree package (./..., <dir>/..., all) or a $(…) list and no narrowing flag.
func goSuite(args []string) bool {
	if len(args) == 0 || args[0] != "test" {
		return false
	}
	tree := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			tree = tree || goTreeArg(a)
			continue
		}
		name, _, attached := strings.Cut(strings.Replace(a, "--", "-", 1), "=")
		if a == "-args" || a == "--args" {
			return tree && !slices.ContainsFunc(args[i+1:], goBinaryNarrows)
		}
		if slices.Contains(goNarrowing, name) || a == "--" {
			return false
		}
		if !attached && slices.Contains(goValueFlags, name) {
			i++
		}
	}
	return tree
}

// goBinaryNarrows is a test-binary flag after -args that selects tests (`-test.run=X`).
func goBinaryNarrows(a string) bool {
	name, _, _ := strings.Cut(strings.Replace(a, "--", "-", 1), "=")
	return slices.Contains([]string{"-test.run", "-test.bench", "-test.list", "-test.fuzz"}, name)
}

// goTreeArg is a package pattern naming many packages: ..., all, <dir>/..., or a $(…) / backtick list of a tree.
func goTreeArg(a string) bool {
	a = strings.TrimSuffix(a, "`") // a backtick list splits into words: its last one closes it
	subst := strings.HasPrefix(a, "$(") || strings.HasPrefix(a, "`")
	return a == "..." || a == "all" || strings.HasSuffix(a, "/...") || subst && strings.Contains(a, "...")
}

// makeValueFlags take the next word as their value, so that word is no target.
var makeValueFlags = []string{"-C", "-f", "-I", "-o", "-W", "--directory", "--file", "--makefile", "--include-dir",
	"--old-file", "--assume-old", "--new-file", "--assume-new", "--what-if", "--eval"}

// makeSuite is a make run naming a test, audit or verify target, not a dry run or a help request.
func makeSuite(args []string) bool {
	hit := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains([]string{"--dry-run", "--just-print", "--recon", "--question", "--help"}, a):
			return false
		case slices.Contains(makeValueFlags, a):
			i++
		case len(a) > 1 && a[0] == '-' && a[1] != '-' && !strings.ContainsAny(a[1:2], "CfIoWjl"):
			if strings.ContainsAny(a, "nqh") { // a short cluster such as -ns: dry run, question or help
				return false
			}
		case !strings.HasPrefix(a, "-") && !isEnvAssign(a):
			hit = hit || a == "test" || a == "audit" || a == "verify"
		}
	}
	return hit
}

// skipValue reports a flag in values given without `=value`, so the next word is its value.
func skipValue(a string, values []string) bool {
	return !strings.Contains(a, "=") && slices.Contains(values, a)
}

// pyStops select tests or print without running the suite.
var pyStops = []string{"-k", "-m", "--deselect", "--lf", "--last-failed", "--sw", "--stepwise", "--collect-only", "--co",
	"--setup-only", "--setup-plan", "--fixtures", "--markers", "--version", "-V", "--help", "-h"}

// pyValueFlags take the next word as their value, so that word is no path.
var pyValueFlags = []string{"-c", "-p", "-o", "-n", "-r", "-W", "--junitxml", "--junit-xml", "--rootdir", "--basetemp",
	"--cov", "--cov-report", "--cov-config", "--dist", "--maxfail", "--durations", "--tb", "--log-level",
	"--confcutdir", "--ignore", "--ignore-glob", "--import-mode", "--capture"}

// pytestSuite is a pytest run over no paths, directories or globs: no -k or -m, no file and no `file::test` node id.
func pytestSuite(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		switch {
		case slices.Contains(pyStops, name) || strings.HasPrefix(a, "-k") || strings.HasPrefix(a, "-m"):
			return false
		case skipValue(a, pyValueFlags):
			i++
		case strings.HasPrefix(a, "-"):
		case !pyTree(a):
			return false
		}
	}
	return true
}

// pyTree reports a path naming a tree: a directory (`dir/`, no extension) or a glob; not a file, node id or $VAR.
func pyTree(a string) bool {
	switch {
	case strings.Contains(a, "::") || strings.ContainsAny(a, "$`"):
		return false
	case strings.ContainsAny(a, "*?") || strings.HasSuffix(a, "/") || a == "." || a == "..":
		return true
	}
	return filepath.Ext(a) == ""
}

// pythonSuite is `python -m pytest <suite args>`, past interpreter flags such as -u.
func pythonSuite(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-m" && args[0] != "-c" {
		if slices.Contains([]string{"-h", "--help", "-V", "--version"}, args[0]) {
			return false
		}
		if (args[0] == "-X" || args[0] == "-W") && len(args) > 1 {
			args = args[1:]
		}
		args = args[1:]
	}
	return len(args) > 1 && args[0] == "-m" && args[1] == "pytest" && pytestSuite(args[2:])
}

// toxStops select an environment, list or print: whether they run the suite is not known, so they allow.
var toxStops = []string{"-e", "--env", "-l", "--listenvs", "-a", "--listenvs-all", "--showconfig", "--notest",
	"--version", "--help", "-h"}

// toxSuite is tox (or `tox run`) with no environment picked and suite-shaped pytest args after `--`.
func toxSuite(args []string) bool {
	if len(args) > 0 && slices.Contains([]string{"run", "r", "run-parallel", "p"}, args[0]) {
		args = args[1:]
	}
	var pos []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, pos = args[:i], args[i+1:]
	}
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "-") || slices.Contains(toxStops, name) || strings.HasPrefix(a, "-e") {
			return false
		}
	}
	return pytestSuite(pos)
}

// jsLeadFlags are package-manager flags before the script that keep the whole run (`pnpm -r test`).
var jsLeadFlags = []string{"-s", "--silent", "-r", "--recursive", "-w", "--workspace-root", "-ws", "--workspaces"}

// jsScripts are the script names that run the test runner.
var jsScripts = []string{"test", "vitest", "jest"}

// scriptSuite is `npm|pnpm|yarn|bun [run] test|test:*|vitest|jest` with suite args, or an exec of vitest or jest.
func scriptSuite(args []string) bool {
	for len(args) > 0 && slices.Contains(jsLeadFlags, args[0]) {
		args = args[1:]
	}
	if len(args) > 0 && (args[0] == "run" || args[0] == "run-script") {
		args = args[1:]
	}
	if len(args) > 0 && (args[0] == "exec" || args[0] == "dlx" || args[0] == "x") {
		return npxSuite(args[1:])
	}
	return len(args) > 0 && (slices.Contains(jsScripts, args[0]) || strings.HasPrefix(args[0], "test:")) && jsSuite(args[1:])
}

// npxSuite is `npx [-y] vitest|jest` with suite args.
func npxSuite(args []string) bool {
	for len(args) > 0 && (args[0] == "-y" || args[0] == "--yes" || args[0] == "--no-install") {
		args = args[1:]
	}
	return len(args) > 0 && (args[0] == "vitest" || args[0] == "jest") && jsSuite(args[1:])
}

// jsStops filter by test name, path pattern, changed files or relation, or print without running.
var jsStops = []string{"-t", "--testNamePattern", "--testPathPattern", "--testPathPatterns", "--findRelatedTests",
	"--related", "--changed", "-o", "--onlyChanged", "--listTests", "--list", "--help", "-h", "--version", "-v"}

// jsValueFlags take the next word as their value, so that word is no file filter.
var jsValueFlags = []string{"--reporter", "--config", "-c", "--environment", "--project", "--shard", "--pool",
	"--maxWorkers", "-w", "--root", "-r", "--dir", "--outputFile", "--testTimeout", "--mode", "--selectProjects"}

// jsSuite is a vitest or jest run with no name filter and no file: only flags and directories.
func jsSuite(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		switch {
		case slices.Contains(jsStops, name) || strings.HasPrefix(a, "-t") && len(a) > 2 && a[2] != '-':
			return false
		case skipValue(a, jsValueFlags):
			i++
		case strings.HasPrefix(a, "-") || a == "run" || a == "watch":
		case !jsDir(a):
			return false
		}
	}
	return true
}

// jsDirNames are the source roots a bare word names as a directory rather than a path pattern.
var jsDirNames = []string{".", "src", "test", "tests", "__tests__", "lib", "app", "packages"}

// jsDir reports a `dir/` or a source root (`./src`); any other word is a path pattern that may pick one file.
func jsDir(a string) bool {
	return strings.HasSuffix(a, "/") || slices.Contains(jsDirNames, strings.TrimPrefix(a, "./"))
}

// cargoStops pick one package, target or kind of test, or compile or print only.
var cargoStops = []string{"-p", "--package", "--test", "--lib", "--bin", "--bins", "--doc", "--example", "--examples",
	"--bench", "--benches", "--no-run", "--list", "--exact", "--ignored", "--help", "-h"}

// cargoValueFlags take the next word as their value, so that word is no test-name filter.
var cargoValueFlags = []string{"-F", "--features", "-j", "--jobs", "--target", "--target-dir", "--manifest-path",
	"--profile", "--exclude", "--color", "-Z", "--config", "--message-format", "--test-threads", "--format"}

// cargoSuite is `cargo [+toolchain] test` with no test-name filter and no package or target picked.
func cargoSuite(args []string) bool {
	if len(args) > 0 && strings.HasPrefix(args[0], "+") {
		args = args[1:]
	}
	if len(args) == 0 || args[0] != "test" {
		return false
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		switch {
		case slices.Contains(cargoStops, name) || strings.HasPrefix(a, "-p") && len(a) > 2:
			return false
		case skipValue(a, cargoValueFlags):
			i++
		case !strings.HasPrefix(a, "-"):
			return false
		}
	}
	return true
}

// mvnValueFlags take the next word as their value, so that word is no goal.
var mvnValueFlags = []string{"-f", "--file", "-l", "--log-file", "-s", "--settings", "-gs", "--global-settings", "-t",
	"--toolchains", "-rf", "--resume-from", "-T", "--threads", "-b", "--builder", "-P", "--activate-profiles"}

// mvnSuite is a Maven run of the test goal with no -Dtest filter, module pick or test skip.
func mvnSuite(args []string) bool {
	hit := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-D" && i+1 < len(args) {
			a, i = "-D"+args[i+1], i+1
		}
		switch {
		case slices.Contains([]string{"-pl", "--projects", "-h", "--help", "-v", "--version"}, a) ||
			strings.HasPrefix(a, "-Dtest=") || strings.HasPrefix(a, "-Dit.test=") || mvnSkips(a):
			return false
		case skipValue(a, mvnValueFlags):
			i++
		case a == "test":
			hit = true
		}
	}
	return hit
}

// mvnSkips is a -DskipTests or -Dmaven.test.skip property not set to false.
func mvnSkips(a string) bool {
	return (strings.HasPrefix(a, "-DskipTests") || strings.HasPrefix(a, "-Dmaven.test.skip")) &&
		!strings.HasSuffix(a, "=false")
}

// gradleValueFlags take the next word as their value, so that word is no task.
var gradleValueFlags = []string{"-p", "--project-dir", "-b", "--build-file", "-c", "--settings-file", "-g",
	"--gradle-user-home", "-I", "--init-script", "--console", "--max-workers", "--include-build", "--warning-mode"}

// gradleSuite is a Gradle run of the test task with no --tests filter; `-x test` excludes it.
func gradleSuite(args []string) bool {
	hit, excluded := false, false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--tests" || strings.HasPrefix(a, "--tests=") || a == "-h" || a == "--help" || a == "--dry-run" || a == "-m":
			return false
		case (a == "-x" || a == "--exclude-task") && i+1 < len(args):
			excluded, i = excluded || args[i+1] == "test", i+1
		case a == "--exclude-task=test" || a == "-xtest":
			excluded = true
		case skipValue(a, gradleValueFlags):
			i++
		case a == "test":
			hit = true
		}
	}
	return hit && !excluded
}
