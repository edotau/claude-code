package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// registryPkgs iterate the hook, setting, verb and schema registries, so any change can break them.
var registryPkgs = []string{"internal/hooks", "internal/settings", "internal/agent", "internal/cli", "internal/schemas"}

// goPkg is the slice of `go list -json` the gate reads.
type goPkg struct {
	ImportPath, Dir                             string
	Imports, TestImports, XTestImports          []string
	EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
	TestGoFiles, XTestGoFiles                   []string
}

// scopedGoGate builds the merge gate: build and vet all, test changed packages and their importers; full = whole suite.
func scopedGoGate(ctx context.Context, dir, base string) (cmd string, pkgs []string, full bool, err error) {
	out, err := runGit(ctx, dir, nil, "diff", "--name-only", "-z", "--no-renames", base+"...HEAD") // both sides of a move
	if err != nil {
		return "", nil, false, fmt.Errorf("git diff %s...HEAD: %w (%s)", base, err, gitErrorDetail(err))
	}
	list, err := listPackages(ctx, dir)
	if err != nil {
		return "", nil, false, err
	}
	abs, err := filepath.Abs(dir) // go list reports absolute package dirs
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs) // ...with symlinks resolved (macOS /var -> /private/var)
	}
	if err != nil {
		return "", nil, false, err
	}
	changed, full := changedPackages(list, abs, string(out))
	if full {
		return fullGoGate, nil, true, nil
	}
	set := reverseDeps(list, abs, changed)
	for _, p := range registryPkgs {
		if goPkgOf(dir, p) == p { // a repo without the harness registries skips them
			set[p] = true
		}
	}
	pkgs = slices.Sorted(maps.Keys(set))
	return scopedGateCommand(pkgs), pkgs, false, nil
}

// changedPackages maps NUL-separated changed files to their package dirs; full is true when the whole suite must run.
func changedPackages(list []goPkg, abs, names string) (changed map[string]bool, full bool) {
	changed, src := map[string]bool{}, cachedReader()
	for f := range strings.SplitSeq(names, "\x00") {
		switch f {
		case "":
		case "go.mod", "go.sum":
			return nil, true
		default:
			owners := filePackages(list, abs, f, src)
			if len(owners) == 0 && !strings.HasSuffix(f, ".md") && !strings.HasSuffix(f, ".go") { // .go: build covers deletions
				return nil, true // an unowned non-markdown file may feed any test
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
	if len(args) == 0 { // a bare `go test` runs the root dir and fails when it has no Go files
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
		p = "" // a subdir's non-Go file is not the root package's unless testdata, embed or a test names it
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
		if !ok || strings.Contains(b, f) || d != "." && strings.Contains(b, d) { // unreadable: assume it refers
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

// reverseDeps returns changed plus every package importing one transitively, plus those whose tests import any.
func reverseDeps(list []goPkg, dir string, changed map[string]bool) map[string]bool {
	rel := map[string]string{} // import path -> repo-relative dir
	for _, p := range list {
		if r, err := filepath.Rel(dir, p.Dir); err == nil {
			rel[p.ImportPath] = filepath.ToSlash(r)
		}
	}
	hit := func(imps []string, in map[string]bool) bool {
		return slices.ContainsFunc(imps, func(i string) bool { return in[rel[i]] })
	}
	affected := propagateImports(list, rel, changed, hit)
	out := map[string]bool{}
	for _, p := range list {
		if r := rel[p.ImportPath]; affected[r] || hit(p.TestImports, affected) || hit(p.XTestImports, affected) {
			out[r] = true
		}
	}
	return out
}

// propagateImports grows changed with every package whose non-test imports reach it, until a fixed point.
func propagateImports(list []goPkg, rel map[string]string, changed map[string]bool,
	hit func([]string, map[string]bool) bool) map[string]bool {
	affected := map[string]bool{}
	for p := range changed {
		affected[p] = true
	}
	for grew := true; grew; { // non-test imports propagate; a test import only selects its own package
		grew = false
		for _, p := range list {
			if r := rel[p.ImportPath]; !affected[r] && hit(p.Imports, affected) {
				affected[r], grew = true, true
			}
		}
	}
	return affected
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
		return "pytest -q", "pyproject"
	}
	if hasNPMTestScript(filepath.Join(dir, "package.json")) {
		return "npm test", "package.json"
	}
	if paths.FileExists(filepath.Join(dir, "Cargo.toml")) {
		return "cargo build && cargo test", "Cargo.toml"
	}
	return "", ""
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
	gate, source = toolchainGate(dir, dirs)
	lint := lintCheck && hasMakeLintCheck(filepath.Join(dir, "Makefile"))
	switch {
	case source == "" && lint:
		return "make lint CHECK=1", "Makefile"
	case source == "":
		return "", "none"
	case lint:
		gate = "make lint CHECK=1 && " + gate
	}
	return gate, source
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

// pythonScopeGate runs pytest (and ruff when configured) over dirs, through .venv when the repo has one.
func pythonScopeGate(dir string, dirs []string) string {
	py := ""
	if info, err := os.Stat(filepath.Join(dir, ".venv")); err == nil && info.IsDir() {
		py = ".venv/bin/python -m "
	}
	targets := shellQuoteAll(dirs)
	pytest := py + "pytest -q"
	if targets != "" {
		pytest += " " + targets
	}
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

// makeCheckRef matches a reference to the CHECK variable itself: $(CHECK), ${CHECK}, $CHECK or $$CHECK.
var makeCheckRef = regexp.MustCompile(`\$(\(CHECK\)|\{CHECK\}|CHECK\b)`)

// hasMakeLintCheck reports whether the lint target's recipe reads CHECK (report-only mode); comments are ignored.
func hasMakeLintCheck(makefile string) bool {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return false
	}
	inRecipe := false
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "lint:"):
			inRecipe = true
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "\t") && inRecipe:
			if code, _, _ := strings.Cut(line, "#"); makeCheckRef.MatchString(code) {
				return true
			}
		default:
			inRecipe = false
		}
	}
	return false
}

// runVerify runs cmdStr under `bash -c "set -o pipefail; ..."`; a nonzero exit is the one signal that stops promotion.
func runVerify(ctx context.Context, workDir, cmdStr string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -o pipefail; "+cmdStr)
	cmd.Dir, cmd.Env = workDir, gateEnv()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	proc.OwnGroup(cmd)
	cmd.WaitDelay = 5 * time.Second // orphans holding the output pipes must not block Wait after cancel
	return cmd.Run()
}

// gateEnv is gitEnv (so a caller's GIT_* hook state cannot redirect the gate's git) minus the vault master key.
func gateEnv() []string {
	return slices.DeleteFunc(gitEnv(), func(kv string) bool { return strings.HasPrefix(kv, "HARNESS_VAULT_KEY=") })
}
