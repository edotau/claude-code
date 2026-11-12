// The formatter chain stop-format drives: format ONE file the way its own project formats it — the
// repo's `make format`/`fmt` target when it honors a per-file variable, else the project's declared
// python chain, else the per-extension tool. Best-effort; every step reports whether it ran cleanly.
package quality

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// toolTimeout bounds one formatter run.
const toolTimeout = 10 * time.Second

// formatFile formats one file via the repo's own format target if any, else the extension chain; true if clean.
func formatFile(path string) bool {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if makeFormat(path) {
		return true
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "go":
		return run("", "gofmt", "-w", path)
	case "py":
		return formatPython(path)
	case "js", "jsx", "mjs", "cjs", "ts", "tsx", "json", "css", "scss", "html", "md", "yaml", "yml":
		root := prettierRoot(path)
		return root != "" && run(root, "prettier", "--write", "--log-level", "warn", path)
	case "sh", "bash":
		return run("", "shfmt", "-w", path)
	}
	return false
}

// Probe order for the Makefile target name and the per-file variable its recipe may honor.
var (
	makeFormatTargets = []string{"format", "fmt"}
	makeFormatVars    = []string{"TARGET", "FILE", "FILES", "PATHS", "SRC"}
)

// makeProvisioning matches recipe commands that BUILD an environment, not format a file (no multi-GB install in Stop).
var makeProvisioning = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(
		`(?i)(-m\s+venv|virtualenv|pip\s+install|uv\s+(venv|sync|pip)|npm\s+(ci|install)|poetry\s+install|conda\s+create)`)
})

// makeProbeTimeout bounds the dry run; the real run gets toolTimeout.
const makeProbeTimeout = 2 * time.Second

// makeFormat formats ONE file through the repo's own format target. A target is used only when it
// demonstrably honors a per-file variable AND a dry run shows it naming this file and provisioning
// nothing: a whole-tree target is skipped, since one edit must never rewrite unrelated files.
func makeFormat(path string) bool {
	root := nearestDirWith(path, "Makefile")
	if root == "" {
		return false
	}
	candidates, ok := makeFormatCandidates(root)
	if !ok {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, candidate := range candidates {
		if runMakeTarget(root, candidate.target, candidate.variable+"="+abs, abs) {
			return true
		}
	}
	return false
}

// makeFormatCandidates returns root's per-file format targets, cached per root; an unreadable Makefile is not cached.
func makeFormatCandidates(root string) ([]makeFormatCandidate, bool) {
	makeFormatMu.Lock()
	defer makeFormatMu.Unlock()
	if candidates, cached := makeFormatCache[root]; cached {
		return candidates, true
	}
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, false
	}
	var candidates []makeFormatCandidate
	for _, target := range makeFormatTargets {
		recipe := makeRecipe(string(body), target)
		for _, name := range makeFormatVars {
			if recipeReads(recipe, name) {
				candidates = append(candidates, makeFormatCandidate{target, name})
			}
		}
	}
	makeFormatCache[root] = candidates
	return candidates, true
}

type makeFormatCandidate struct{ target, variable string }

var makeFormatMu sync.Mutex
var makeFormatCache = map[string][]makeFormatCandidate{}

// makeRecipe returns the tab-indented recipe lines for target, or "" when the target is absent.
func makeRecipe(body, target string) string {
	var recipe []string
	collecting := false
	for line := range strings.SplitSeq(body, "\n") {
		if after, ok := strings.CutPrefix(line, "\t"); ok {
			if collecting {
				recipe = append(recipe, after)
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue // blank/comment lines do not end a recipe
		}
		collecting = isTargetLine(trimmed, target)
	}
	return strings.Join(recipe, "\n")
}

// isTargetLine reports whether a Makefile line declares target (`format:` / `format : deps`), not `format := ...`.
func isTargetLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, target)
	if !ok {
		return false
	}
	rest = strings.TrimLeft(rest, " \t")
	return strings.HasPrefix(rest, ":") && !strings.HasPrefix(rest, ":=")
}

// recipeReads reports whether the recipe expands the named variable.
func recipeReads(recipe, name string) bool {
	return strings.Contains(recipe, "$("+name+")") || strings.Contains(recipe, "${"+name+"}")
}

// runMakeTarget dry-runs the target and, only if every planned command names this file and provisions
// nothing, runs it for real. A line naming other paths (e.g. `gofmt -w .`) would rewrite the whole tree.
func runMakeTarget(root, target, assignment, abs string) bool {
	plan, ok := runMake(root, makeProbeTimeout, "-n", "-s", target, assignment)
	if !ok || makeProvisioning().MatchString(plan) || !everyLineNames(root, plan, abs) {
		return false
	}
	_, ok = runMake(root, toolTimeout, "-s", target, assignment)
	return ok
}

// everyLineNames reports whether plan has at least one line and each non-blank line passes namesOnly.
func everyLineNames(root, plan, abs string) bool {
	named := false
	for line := range strings.SplitSeq(plan, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !namesOnly(root, strings.Fields(line), abs) {
			return false
		}
		named = true
	}
	return named
}

// namesOnly reports whether abs is a whole argument of the command and no other argument is path-like.
func namesOnly(root string, fields []string, abs string) bool {
	names := false
	for i, tok := range fields {
		if strings.ContainsAny(tok, ";&|<>`$()\\") {
			return false // a chained, substituted or redirected command is not just this file's formatter
		}
		tok = strings.Trim(tok, `"'`)
		if tok == abs {
			names = true
			continue
		}
		if i == 0 || strings.HasPrefix(tok, "-") {
			continue // the command word and flags are not paths
		}
		if pathLike(root, tok) {
			return false
		}
	}
	return names
}

// pathLike reports an argument that can name files beyond the target: a separator, glob, dot, or a tree entry.
func pathLike(root, tok string) bool {
	if strings.ContainsAny(tok, "/*.") {
		return true
	}
	_, err := os.Stat(filepath.Join(root, tok))
	return err == nil
}

// runMake executes make in root under a timeout, returning its combined output and whether it exited 0.
func runMake(root string, timeout time.Duration, args ...string) (string, bool) {
	mk, err := exec.LookPath("make")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, mk, args...)
	cmd.Dir = root
	proc.OwnGroup(cmd)          // the timeout kills make's whole group, so a recipe's formatter cannot outlive it
	cmd.WaitDelay = time.Second // grandchildren holding the output pipe must not outlive the timeout
	// MAKEFLAGS from a parent make run would leak -n or a jobserver into this invocation.
	cmd.Env = append(os.Environ(), "MAKEFLAGS=", "MFLAGS=", "MAKELEVEL=")
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

var toolSection = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*\[tool\.([A-Za-z0-9_-]+)[.\]]`)
})

// formatPython formats one .py file: the declared classic chain (black/isort/autoflake) if installed, else ruff.
func formatPython(path string) bool {
	root, tools := pythonProject(path)
	if root != "" && (tools["black"] || tools["isort"] || tools["autoflake"]) {
		if ran, ok := classicPythonChain(root, path, tools); ran {
			return ok
		}
	}
	// ruff-only chain: check --fix covers autoflake (F401/F841) + isort (I); format is black-compatible.
	run(root, "ruff", "check", "--fix", "--quiet", path) // exits 1 on lint it cannot fix, so only format decides clean
	return run(root, "ruff", "format", "--quiet", path)
}

// classicPythonChain runs ruff check --fix, autoflake, isort, black — each only when the project declares
// it and it is installed, each from the project dir. ran: any step ran; ok: every step that ran exited 0.
func classicPythonChain(root, path string, tools map[string]bool) (ran, ok bool) {
	ok = true
	step := func(name string, args ...string) {
		if resolveTool(root, name) == "" {
			return
		}
		ran = true
		ok = run(root, name, args...) && ok // run before the && so every step executes
	}
	if tools["ruff"] {
		step("ruff", "check", "--fix", "--quiet", path)
	}
	if tools["autoflake"] {
		// --in-place explicitly: without the project's [tool.autoflake] in reach autoflake only prints.
		step("autoflake", "--in-place", "--quiet", path)
	}
	if tools["isort"] {
		step("isort", "--quiet", path)
	}
	if tools["black"] {
		step("black", "--quiet", path)
	}
	return ran, ok
}

// pythonProject walks up to the nearest pyproject.toml and returns its dir plus declared [tool.*] sections; "" if none.
func pythonProject(path string) (string, map[string]bool) {
	root := nearestDirWith(path, "pyproject.toml")
	if root == "" {
		return "", nil
	}
	body, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return "", nil
	}
	tools := map[string]bool{}
	for _, m := range toolSection().FindAllSubmatch(body, -1) {
		tools[string(m[1])] = true
	}
	return root, tools
}

// resolveTool prefers the project's .venv and node_modules/.bin, then PATH; "" when absent.
func resolveTool(root, name string) string {
	if root != "" {
		for _, bin := range []string{filepath.Join(".venv", "bin"), filepath.Join("node_modules", ".bin")} {
			if p := filepath.Join(root, bin, name); isExec(p) {
				return p
			}
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// run executes a formatter from dir under toolTimeout; reports whether it ran AND exited 0.
func run(dir, name string, args ...string) bool {
	p := resolveTool(dir, name)
	if p == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, args...)
	cmd.Dir = dir
	proc.OwnGroup(cmd)          // the timeout kills the formatter's whole group, wrapper-spawned children included
	cmd.WaitDelay = time.Second // grandchildren holding the output pipe must not outlive the timeout
	return cmd.Run() == nil
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// nearestDirWith returns the closest ancestor (within 8 levels) of path holding name.
func nearestDirWith(path, name string) string {
	return walkUp(path, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	})
}

// walkUp returns the closest ancestor (within 8 levels) of path for which match holds, or "".
func walkUp(path string, match func(dir string) bool) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return ""
	}
	for range 8 {
		if match(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// prettierConfigs are the file names that declare a prettier setup for a project.
var prettierConfigs = []string{
	".prettierrc", ".prettierrc.json", ".prettierrc.yaml", ".prettierrc.yml", ".prettierrc.json5", ".prettierrc.js",
	".prettierrc.cjs", ".prettierrc.mjs", ".prettierrc.toml", ".prettierrc.ts", ".prettierrc.mts", ".prettierrc.cts",
	"prettier.config.js", "prettier.config.cjs", "prettier.config.mjs", "prettier.config.ts", "prettier.config.mts",
	"prettier.config.cts",
}

// prettierRoot is the nearest ancestor (≤8 levels) with a prettier config or package.json entry, or "".
func prettierRoot(path string) string {
	return walkUp(path, func(dir string) bool {
		for _, name := range prettierConfigs {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return true
			}
		}
		return packageDeclaresPrettier(filepath.Join(dir, "package.json"))
	})
}

// packageDeclaresPrettier reports a package.json with a "prettier" key or a prettier (dev)dependency.
func packageDeclaresPrettier(pkgPath string) bool {
	body, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		Prettier        json.RawMessage   `json:"prettier"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(body, &pkg) != nil {
		return false
	}
	_, dep := pkg.Dependencies["prettier"]
	_, devDep := pkg.DevDependencies["prettier"]
	return len(pkg.Prettier) > 0 || dep || devDep
}
