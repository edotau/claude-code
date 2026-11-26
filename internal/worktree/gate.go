package worktree

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/textutil"
)

// goPkg is the slice of `go list -json` the gate reads.
type goPkg struct {
	ImportPath, Dir                             string
	Imports, TestImports, XTestImports          []string
	EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
	TestGoFiles, XTestGoFiles                   []string
}

// scopedGoGate builds the land gate: build and vet all, test the nChanged changed packages and their direct importers.
func scopedGoGate(ctx context.Context, dir, base string) (cmd string, pkgs []string, nChanged int, sc gateScope, err error) {
	out, err := runGit(ctx, dir, nil, "diff", "--name-only", "-z", "--no-renames", base+"...HEAD")
	if err != nil {
		return "", nil, 0, sc, fmt.Errorf("git diff %s...HEAD: %w (%s)", base, err, gitErrorDetail(err))
	}
	list, err := listPackages(ctx, dir)
	if err != nil {
		return "", nil, 0, sc, err
	}
	// go list reports absolute package dirs
	abs, err := filepath.Abs(dir)
	if err == nil {
		// ...with symlinks resolved (macOS /var -> /private/var)
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", nil, 0, sc, err
	}
	changed, full := changedPackages(list, abs, string(out))
	if full {
		return fullGoGate, nil, 0, sc, nil
	}
	set := reverseDeps(list, abs, changed)
	pkgs = slices.Sorted(maps.Keys(set))
	for p := range changed {
		if set[p] {
			nChanged++
		}
	}
	return scopedGateCommand(pkgs), pkgs, nChanged, gateScope{Changed: changed, Tested: set, Scoped: true}, nil
}

// changedPackages maps NUL-separated changed files to their package dirs; full is true when the whole suite must run.
func changedPackages(list []goPkg, abs, names string) (changed map[string]bool, full bool) {
	changed, src := map[string]bool{}, cachedReader()
	for _, f := range textutil.SplitNUL(names) {
		switch f {
		case "go.mod", "go.sum":
			return nil, true
		default:
			owners := filePackages(list, abs, f, src)
			// .go: build covers deletions
			if len(owners) == 0 && !strings.HasSuffix(f, ".md") && !strings.HasSuffix(f, ".go") {
				// an unowned non-markdown file may feed any test
				return nil, true
			}
			for _, p := range owners {
				changed[p] = true
			}
		}
	}
	return changed, false
}

// scopedGateCommand renders the build, vet and test command line for pkgs.
func scopedGateCommand(pkgs []string) string {
	args := mapStrings(pkgs, func(p string) string { return textutil.QuoteSH("./" + p) })
	// a bare `go test` runs the root dir and fails when it has no Go files
	if len(args) == 0 {
		return "go build ./... && go vet ./..."
	}
	return "go build ./... && go vet ./... && go test " + strings.Join(args, " ")
}

const fullGoGate = "go build ./... && go vet ./... && go test ./..."

// filePackages maps a changed file to its package dir; a non-Go file also to packages embedding it or tests naming it.
func filePackages(list []goPkg, abs, f string, src func(string) (string, bool)) []string {
	var out []string
	dir, isGo := filepath.Dir(f), strings.HasSuffix(f, ".go")
	p := goPkgOf(abs, dir)
	if p == "." && dir != "." && !isGo && !strings.Contains("/"+f, "/testdata/") {
		// a subdir's non-Go file is not the root package's unless testdata, embed or a test names it
		p = ""
	}
	if p != "" {
		out = append(out, p)
	}
	if isGo {
		return out
	}
	for _, p := range list {
		if r, err := filepath.Rel(abs, p.Dir); err == nil && refersTo(p, filepath.ToSlash(r), f, src) {
			out = append(out, filepath.ToSlash(r))
		}
	}
	return out
}

// refersTo reports whether p (at repo-relative r) embeds f or has a test file mentioning f or its directory.
func refersTo(p goPkg, r, f string, src func(string) (string, bool)) bool {
	for _, e := range slices.Concat(p.EmbedFiles, p.TestEmbedFiles, p.XTestEmbedFiles) {
		if path.Join(r, filepath.ToSlash(e)) == f {
			return true
		}
	}
	d := path.Dir(f)
	for _, t := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
		b, ok := src(filepath.Join(p.Dir, t))
		// unreadable: assume it refers
		if !ok || strings.Contains(b, f) || d != "." && strings.Contains(b, d) {
			return true
		}
	}
	return false
}

// cachedReader returns a file reader that reads each path once; ok is false for an unreadable file.
func cachedReader() func(string) (string, bool) {
	type res struct {
		b  string
		ok bool
	}
	seen := map[string]res{}
	return func(p string) (string, bool) {
		if r, hit := seen[p]; hit {
			return r.b, r.ok
		}
		b, err := os.ReadFile(p)
		seen[p] = res{string(b), err == nil}
		return seen[p].b, err == nil
	}
}

// listPackages runs `go list -e -json ./...` in dir and decodes its package stream.
func listPackages(ctx context.Context, dir string) ([]goPkg, error) {
	c := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	c.Dir, c.Env = dir, gateEnv()
	raw, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w (%s)", err, gitErrorDetail(err))
	}
	var pkgs []goPkg
	for dec := json.NewDecoder(strings.NewReader(string(raw))); ; {
		var p goPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list ./...: %w", err)
		}
		pkgs = append(pkgs, p)
	}
}

// reverseDeps returns the changed packages that exist plus those importing one directly (non-test or test imports).
func reverseDeps(list []goPkg, dir string, changed map[string]bool) map[string]bool {
	// import path -> repo-relative dir
	rel := map[string]string{}
	for _, p := range list {
		if r, err := filepath.Rel(dir, p.Dir); err == nil {
			rel[p.ImportPath] = filepath.ToSlash(r)
		}
	}
	hit := func(imps []string) bool {
		return slices.ContainsFunc(imps, func(i string) bool { return changed[rel[i]] })
	}
	out := map[string]bool{}
	for _, p := range list {
		if r := rel[p.ImportPath]; changed[r] || hit(p.Imports) || hit(p.TestImports) || hit(p.XTestImports) {
			out[r] = true
		}
	}
	return out
}

// goPkgOf returns the nearest existing directory at or above rel under dir that holds .go files, or "".
func goPkgOf(dir, rel string) string {
	for rel != "" && rel != "/" {
		if ents, err := os.ReadDir(filepath.Join(dir, rel)); err == nil && slices.ContainsFunc(ents, isGoFile) {
			return filepath.ToSlash(rel)
		}
		if rel == "." {
			break
		}
		rel = filepath.Dir(rel)
	}
	return ""
}

func isGoFile(e os.DirEntry) bool { return !e.IsDir() && strings.HasSuffix(e.Name(), ".go") }

// chooseVerifyGate picks the post-integration gate: noVerify skips it, an explicit verify wins, else it is detected.
func chooseVerifyGate(worktreePath, verify string, noVerify bool) (gate, source string, err error) {
	if noVerify {
		return "", "", nil
	}
	if verify != "" {
		return verify, "flag", nil
	}
	gate, source = detectVerifyGate(worktreePath)
	if gate == "" {
		return "", "", errors.New(`no verification gate found — pass --verify "<cmd>" or --no-verify`)
	}
	return gate, source, nil
}

// detectVerifyGate finds a suite-wide gate: Makefile test, go.mod, pyproject/setup.cfg, package.json, Cargo.toml.
func detectVerifyGate(dir string) (gate, source string) {
	if hasMakeTarget(filepath.Join(dir, "Makefile"), "test") {
		return "make test", "Makefile"
	}
	if paths.FileExists(filepath.Join(dir, "go.mod")) {
		return fullGoGate, "go.mod"
	}
	if paths.FileExists(filepath.Join(dir, "pyproject.toml")) || paths.FileExists(filepath.Join(dir, "setup.cfg")) {
		return "(" + venvPython(dir) + "pytest)", "pyproject"
	}
	if hasNPMTestScript(filepath.Join(dir, "package.json")) {
		return "npm test", "package.json"
	}
	if paths.FileExists(filepath.Join(dir, "Cargo.toml")) {
		return "cargo build && cargo test", "Cargo.toml"
	}
	return "", ""
}

// pytestNoTestsExit is pytest's exit code for an empty collection.
const pytestNoTestsExit = 5

// errNoTests fails a lane that changed a .py file yet collected no tests.
var errNoTests = errors.New("verify: no tests collected")

// emptyPytest settles pytest's empty-collection exit: fails on a lane .py or pytest-config change, else passes.
func emptyPytest(ctx context.Context, dir, base string, err error, out io.Writer) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != pytestNoTestsExit {
		return err
	}
	if base == "" {
		return errNoTests
	}
	names, gitErr := runGit(ctx, dir, nil, "diff", "--name-only", "-z", "--no-renames", base+"...HEAD")
	isPy := func(f string) bool {
		return strings.HasSuffix(f, ".py") || slices.Contains([]string{"pyproject.toml", "setup.cfg", "pytest.ini", "tox.ini"}, path.Base(f))
	}
	if gitErr != nil || slices.ContainsFunc(textutil.SplitNUL(string(names)), isPy) {
		return errNoTests
	}
	fmt.Fprintln(out, "verify: no tests collected (no .py or pytest-config change in the lane); passing")
	return nil
}

// hasMakeTarget reports whether makefile declares a target named exactly target; a missing Makefile is "no target".
func hasMakeTarget(makefile, target string) bool {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return false
	}
	return strings.Contains("\n"+string(data), "\n"+target+":")
}

// hasNPMTestScript reports whether pkgJSON declares a "test" script.
func hasNPMTestScript(pkgJSON string) bool {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	_, ok := pkg.Scripts["test"]
	return ok
}

// ScopedVerifyGate renders the gate for dir's toolchain, limited to scope dirs; source "none" when nothing fits.
func ScopedVerifyGate(dir string, scope []string, lintCheck bool) (gate, source string) {
	dirs := normalizeScope(dir, scope)
	if len(dirs) > 0 && paths.FileExists(filepath.Join(dir, "go.mod")) {
		if dirs = slices.DeleteFunc(dirs, func(d string) bool { return noGo(filepath.Join(dir, d)) }); len(dirs) == 0 {
			return "", "none"
		}
	}
	gate, source = toolchainGate(dir, dirs)
	if source == "" {
		return "", "none"
	}
	// the verb needs a Go module and a Makefile lint target (today's probe); other repos get no lint part
	if lintCheck && paths.FileExists(filepath.Join(dir, "go.mod")) && hasMakeTarget(filepath.Join(dir, "Makefile"), "lint") {
		gate = lintLine(lintVerb(dirs)) + " && " + gate
	}
	return gate, source
}

// noGo reports whether d exists with no .go file at or under it; go vet and test fail on such a pattern.
func noGo(d string) bool {
	if _, err := os.Stat(d); err != nil {
		return false
	}
	found := false
	_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(p, ".go") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return !found
}

// landBaseEnv carries the land's pinned target OID into the gate; unset, the lint compares against HEAD.
const landBaseEnv = "HARNESS_LAND_BASE"

// lintVerb is the lint set `make lint` runs, as the agent verb: the whole tree, or the scope's changes since the base.
func lintVerb(dirs []string) []string {
	argv := []string{"claude-code", "agents", "diff", "--check", "--gates", "lint"}
	if len(dirs) == 0 {
		return append(argv, "--all")
	}
	// a merged lane is committed, so HEAD alone shows no change
	argv = append(argv, "--base", lintBase)
	for _, d := range dirs {
		argv = append(argv, "--path", d)
	}
	return argv
}

// lintBase is the one word the gate shell expands: the pinned land base, else HEAD.
const lintBase = "${" + landBaseEnv + ":-HEAD}"

// lintLine renders the lint argv as gate shell words; only the value of --base stays expandable.
func lintLine(argv []string) string {
	words := mapStrings(argv, textutil.QuoteSH)
	for i, w := range argv {
		if w == lintBase && i > 0 && argv[i-1] == "--base" {
			words[i] = `"` + w + `"`
		}
	}
	return strings.Join(words, " ")
}

// normalizeScope slash-cleans scope dirs ("."/"" = whole repo); a leading - keeps its ./ so it is never an option.
func normalizeScope(dir string, scope []string) []string {
	var out []string
	for _, d := range scope {
		d = strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(relativeScope(dir, strings.TrimSpace(d))), "./"), "/")
		if d == "" || d == "." {
			return nil
		}
		if strings.HasPrefix(d, "-") {
			d = "./" + d
		}
		out = append(out, d)
	}
	return out
}

// relativeScope makes an absolute scope dir inside dir (symlinks resolved on both) repo-relative; others pass through.
func relativeScope(dir, d string) string {
	if !filepath.IsAbs(d) {
		return d
	}
	for _, resolve := range []func(string) string{filepath.Clean, paths.Canon} {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return d
		}
		if r, err := filepath.Rel(resolve(abs), resolve(d)); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return r
		}
	}
	return d
}

// toolchainGate returns the scoped gate of the first toolchain file in dir; "" and "" when none or it cannot scope.
func toolchainGate(dir string, dirs []string) (gate, source string) {
	switch {
	case paths.FileExists(filepath.Join(dir, "go.mod")):
		return goScopeGate(dirs), "go.mod"
	case paths.FileExists(filepath.Join(dir, "pyproject.toml")) || paths.FileExists(filepath.Join(dir, "setup.cfg")):
		return pythonScopeGate(dir, dirs), "pyproject"
	case hasNPMTestScript(filepath.Join(dir, "package.json")):
		return npmScopeGate(dirs), "package.json"
	case paths.FileExists(filepath.Join(dir, "Cargo.toml")) && len(dirs) == 0:
		// cargo test filters by test name, not directory, so a scoped run would be unscoped: refuse it
		return "cargo build && cargo test", "Cargo.toml"
	}
	return "", ""
}

// npmScopeGate runs the package.json test script, passing dirs through as path arguments (jest/vitest style).
func npmScopeGate(dirs []string) string {
	if len(dirs) == 0 {
		return "npm test"
	}
	return "npm test -- " + shellQuoteAll(dirs)
}

// goScopeGate builds, vets and tests the module-relative dirs; no dirs means the whole suite.
func goScopeGate(dirs []string) string {
	if len(dirs) == 0 {
		return fullGoGate
	}
	p := shellQuoteAll(mapStrings(dirs, func(d string) string { return "./" + d + "/..." }))
	return "go build ./... && go vet " + p + " && go test " + p
}

// Pytest gates are subshells so rtk leaves them bare: rtk adds -q, which with addopts -q reads "No tests collected".

// isPytestStep reports whether the last command of step is a "(...pytest...)" subshell, whose exit 5 is pytest's.
func isPytestStep(step string) bool {
	_, last, found := strings.Cut(step, " && ")
	if !found {
		last = step
	}
	return strings.HasPrefix(strings.TrimSpace(last), "(") && strings.Contains(last, "pytest")
}

// venvPython is the `-m` prefix of dir's .venv or venv interpreter, empty when it has neither.
func venvPython(dir string) string {
	for _, v := range []string{".venv", "venv"} {
		if paths.FileExists(filepath.Join(dir, v, "bin", "python")) {
			return v + "/bin/python -m "
		}
	}
	return ""
}

// pythonScopeGate runs pytest (and ruff when configured) over dirs, through its .venv or venv when it has one.
func pythonScopeGate(dir string, dirs []string) string {
	py := venvPython(dir)
	targets := shellQuoteAll(dirs)
	pytest := py + "pytest"
	if targets != "" {
		pytest += " " + targets
	}
	pytest = "(" + pytest + ")"
	if !hasRuffConfig(dir) {
		return pytest
	}
	if targets == "" {
		targets = "."
	}
	return py + "ruff check " + targets + " && " + pytest
}

// shellQuoteAll single-quotes each item for `bash -c` and joins them with spaces.
func shellQuoteAll(items []string) string {
	return strings.Join(mapStrings(items, textutil.ShellQuote), " ")
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

// hasRuffConfig reports whether dir configures ruff via ruff.toml, .ruff.toml or a [tool.ruff] pyproject table.
func hasRuffConfig(dir string) bool {
	if paths.FileExists(filepath.Join(dir, "ruff.toml")) || paths.FileExists(filepath.Join(dir, ".ruff.toml")) {
		return true
	}
	data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	return err == nil && hasRuffTable(string(data))
}

// hasRuffTable reports whether a TOML line is a [tool.ruff] or [tool.ruff.*] table header.
func hasRuffTable(toml string) bool {
	for line := range strings.SplitSeq(toml, "\n") {
		h := strings.TrimSpace(line)
		if strings.HasPrefix(h, "[tool.ruff]") || strings.HasPrefix(h, "[tool.ruff.") {
			return true
		}
	}
	return false
}

// runVerify runs cmdStr under `bash -c "set -o pipefail; ..."`; a nonzero exit is the one signal that stops promotion.
func runVerify(ctx context.Context, workDir, cmdStr, base string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -o pipefail; "+rtkwrap.ForModel(ctx, cmdStr, workDir))
	cmd.Dir, cmd.Env = workDir, append(gateEnv(), landBaseEnv+"="+base)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	proc.TreeCancel(cmd, gateCancelGrace) // cancel stops the whole tree, including children in their own groups
	// orphans holding the output pipes must not block Wait after cancel
	cmd.WaitDelay = gateWaitDelay
	return cmd.Run()
}

var gateWaitDelay = 5 * time.Second

const gateCancelGrace = 2 * time.Second

// gateEnv is gitEnv minus the vault master key, with -buildvcs=false -trimpath in GOFLAGS (shared build-cache keys).
func gateEnv() []string {
	env := slices.DeleteFunc(gitEnv(), func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") })
	return proc.GoFlagsEnv(env, "-buildvcs=false", "-trimpath")
}

// scopedGateSource prefixes the source of a gate scopeGate built from independent steps.
const scopedGateSource = "scoped go gate"

// gateStages orders a go gate (scoped or the full suite) as [build] then [vet, test]; any other gate is one stage.
func gateStages(gate, source string) [][]string {
	if !usesGateSlot(gate, source) {
		return [][]string{{gate}}
	}
	steps := strings.Split(gate, " && ")
	if len(steps) < 2 {
		return [][]string{steps}
	}
	return [][]string{steps[:1], steps[1:]}
}

// stepName is the stage label of a step: "go vet ./..." is "vet", anything else its first word.
func stepName(step string) string {
	f := strings.Fields(strings.TrimPrefix(step, "go "))
	if len(f) == 0 {
		return step
	}
	return f[0]
}

// gateAt is where a gate runs (Dir), the land base it compares against and the lane kept on a failure.
type gateAt struct{ Dir, Base, Lane string }

// runStages runs stages in order, a stage's steps concurrently; returns seconds per step, a failure names its stage.
func runStages(ctx context.Context, at gateAt, stages [][]string, stdout, stderr io.Writer) (map[string]float64, error) {
	secs := map[string]float64{}
	for _, steps := range stages {
		durs, err := runTimed(ctx, at, steps, stdout, stderr)
		names := mapStrings(steps, stepName)
		for i, d := range durs {
			secs[names[i]] += d.Seconds()
		}
		if err != nil {
			return secs, fmt.Errorf("%s stage: %w", strings.Join(names, "+"), err)
		}
	}
	return secs, nil
}

// runTimed runs steps concurrently, each with its own output buffer; the first failure cancels the rest.
func runTimed(ctx context.Context, at gateAt, steps []string, stdout, stderr io.Writer) ([]time.Duration, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	outs, errs, fails := make([]bytes.Buffer, len(steps)), make([]bytes.Buffer, len(steps)), make([]error, len(steps))
	durs := make([]time.Duration, len(steps))
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for i, s := range steps {
		wg.Go(func() {
			start := time.Now()
			fails[i] = runVerify(ctx, at.Dir, s, at.Base, &outs[i], &errs[i])
			durs[i] = time.Since(start)
			if fails[i] != nil && isPytestStep(s) {
				fails[i] = emptyPytest(ctx, at.Dir, at.Base, fails[i], &outs[i])
			}
			if fails[i] != nil {
				// the first to fail is the cause; the rest die from its cancel
				once.Do(func() { first = fmt.Errorf("%s: %w", s, fails[i]) })
				cancel()
			}
		})
	}
	wg.Wait()
	for i, s := range steps {
		if len(steps) > 1 {
			fmt.Fprintf(stdout, "--- %s\n", s)
		}
		_, _ = outs[i].WriteTo(stdout)
		_, _ = errs[i].WriteTo(stderr)
	}
	return durs, first
}

// gateFailure is one failing location parsed from gate output; file is empty when the line names none.
type gateFailure struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	// DiagnosticID is a short hash of file and normalized text (not line), unique within a run.
	DiagnosticID string `json:"diagnostic_id"`
}

// GateRun is a scoped gate's observed result: real exit (nil if it never ran), changed files, failures.
type GateRun struct {
	Gate    string   `json:"gate"`
	Source  string   `json:"source"`
	Exit    *int     `json:"exit"`
	Changed []string `json:"changed"`
	// ChangeIDs[i] is the short hash of Changed[i], so it moves when the file's content does.
	ChangeIDs []string      `json:"change_ids"`
	Failures  []gateFailure `json:"failures"`
}

const gateRunTimeout = 20 * time.Minute

var gateLocRe = regexp.MustCompile(`^\s*([^:\s][^:]*\.\w+):(\d+)(?::\d+)?:?\s*(.*)$`)

// RunScopedGate runs the scoped gate for dir once and returns code-observed exit, changed paths and failures.
func RunScopedGate(ctx context.Context, dir string, scope []string, lintCheck bool) (GateRun, error) {
	gate, source := ScopedVerifyGate(dir, scope, lintCheck)
	if gate == "" {
		return GateRun{Source: source}, fmt.Errorf("no verification gate found in %s", dir)
	}
	ctx, cancel := context.WithTimeout(ctx, gateRunTimeout)
	defer cancel()
	var out bytes.Buffer
	err := runVerify(ctx, dir, gate, "", &out, &out)
	r := GateRun{Gate: gate, Source: source, Failures: []gateFailure{}}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return r, fmt.Errorf("gate did not finish: %w", ctx.Err())
	case err == nil:
		r.Exit = new(int)
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		code := ee.ExitCode()
		r.Exit = &code
		r.Failures = parseGateFailures(out.String())
	default:
		return r, err
	}
	var cerr error
	if r.Changed, cerr = gateChanged(ctx, dir, scope); cerr != nil {
		return r, cerr
	}
	r.ChangeIDs = mapStrings(r.Changed, func(c string) string { return shortID(c) })
	return r, nil
}

// shortID is the first 12 hex digits of the SHA-256 of the NUL-joined parts.
func shortID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

var gateNoiseRe = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|s)\b|(?:/private)?/(?:tmp|var/folders)/[^ :]+(?::\d+){0,2}`)

// assignDiagnosticIDs stamps each failure; repeats of one file and text mix in their occurrence number (run-local).
func assignDiagnosticIDs(fails []gateFailure) {
	seen := map[string]int{}
	for i := range fails {
		first, _, _ := strings.Cut(fails[i].Text, "\n")
		key := fails[i].File + "\x00" + strings.TrimSpace(gateNoiseRe.ReplaceAllString(first, ""))
		n := seen[key]
		seen[key]++
		fails[i].DiagnosticID = shortID(key, strconv.Itoa(n))
	}
}

// parseGateFailures reads file:line: text lines; red output with none keeps its first FAIL (else last) line.
func parseGateFailures(out string) []gateFailure {
	fails := []gateFailure{}
	var last, firstFail string
	for line := range strings.SplitSeq(out, "\n") {
		if m := gateLocRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[2])
			fails = append(fails, gateFailure{File: strings.TrimPrefix(m[1], "./"), Line: n, Text: strings.TrimSpace(m[3])})
		} else if t := strings.TrimSpace(line); t != "" {
			if last = t; firstFail == "" && strings.Contains(t, "FAIL") {
				firstFail = t
			}
		}
	}
	if len(fails) == 0 && last != "" {
		fails = append(fails, gateFailure{Text: cmp.Or(firstFail, last)})
	}
	assignDiagnosticIDs(fails)
	return fails
}

// gateChanged lists `<path> <hash|deleted>` for dirty files under scope (whole repo when empty).
func gateChanged(ctx context.Context, dir string, scope []string) ([]string, error) {
	probe := gitCmd(ctx, "rev-parse", "--is-inside-work-tree")
	probe.Dir = dir
	var pe *exec.ExitError
	if err := probe.Run(); errors.As(err, &pe) && pe.ExitCode() == 128 {
		return []string{}, nil // not a git repo: nothing is observable as changed
	} else if err != nil {
		return nil, fmt.Errorf("git rev-parse: %w", err)
	}
	args := []string{"status", "--porcelain", "-z", "--untracked-files=all"}
	if len(scope) > 0 {
		args = append(append(args, "--"), scope...)
	}
	cmd := gitCmd(ctx, args...)
	cmd.Dir = dir
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	changed := []string{}
	recs := strings.Split(string(data), "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		p := rec[3:]
		if strings.ContainsAny(rec[:2], "RC") {
			i++ // the rename source follows as its own record
		}
		changed = append(changed, gateStamp(ctx, dir, p))
	}
	return changed, nil
}

// gateStamp is `<path> <hash>`, or `<path> deleted`; the bare path when hashing is refused.
func gateStamp(ctx context.Context, dir, p string) string {
	if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
		return p + " deleted"
	}
	h := gitCmd(ctx, "hash-object", "--", p)
	h.Dir = dir
	if sum, err := h.Output(); err == nil {
		return p + " " + strings.TrimSpace(string(sum))
	}
	return p
}
