// The formatter chain stop-format drives: format ONE file the way its own project formats it — the
// repo's `make format`/`fmt` target when it honors a per-file variable, else the project's declared
// python chain, else the per-extension tool. Best-effort; every step reports whether it ran cleanly.
package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// toolTimeout bounds one formatter run.
const toolTimeout = 10 * time.Second

// formatFile formats one file, preferring the repo's own format target so each project formats by its
// own config; the extension chain is the fallback. Reports whether a formatter ran cleanly.
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
		return run("", "prettier", "--write", "--log-level", "warn", path)
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

// makeProvisioning matches recipe commands that BUILD an environment rather than format a file: a
// missing .venv must not turn one edit into a multi-GB install inside the Stop hook's budget.
var makeProvisioning = regexp.MustCompile(
	`(?i)(-m\s+venv|virtualenv|pip\s+install|uv\s+(venv|sync|pip)|npm\s+(ci|install)|poetry\s+install|conda\s+create)`)

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
	makeFormatMu.Lock()
	candidates, cached := makeFormatCache[root]
	if !cached {
		body, _ := os.ReadFile(filepath.Join(root, "Makefile"))
		for _, target := range makeFormatTargets {
			recipe := makeRecipe(string(body), target)
			for _, name := range makeFormatVars {
				if recipeReads(recipe, name) {
					candidates = append(candidates, makeFormatCandidate{target, name})
				}
			}
		}
		makeFormatCache[root] = candidates
	}
	makeFormatMu.Unlock()
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

// isTargetLine reports whether a Makefile line declares target (`format:` / `format : deps`), excluding
// variable assignments (`format := ...`).
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

// runMakeTarget dry-runs the target and, only if the plan names this file and provisions nothing, runs
// it for real. The plan must name the file: otherwise the variable is read somewhere that does not
// reach it (or the target formats fixed paths) and a real run would touch the whole tree.
func runMakeTarget(root, target, assignment, abs string) bool {
	plan, ok := runMake(root, makeProbeTimeout, "-n", "-s", target, assignment)
	if !ok || makeProvisioning.MatchString(plan) || !strings.Contains(plan, abs) {
		return false
	}
	_, ok = runMake(root, toolTimeout, "-s", target, assignment)
	return ok
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
	// MAKEFLAGS from a parent make run would leak -n or a jobserver into this invocation.
	cmd.Env = append(os.Environ(), "MAKEFLAGS=", "MFLAGS=", "MAKELEVEL=")
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

var toolSection = regexp.MustCompile(`(?m)^\s*\[tool\.([A-Za-z0-9_-]+)[.\]]`)

// formatPython formats one .py file: the project's declared classic chain when its pyproject declares
// any of black/isort/autoflake, else (or when none of the declared tools is installed) the ruff chain.
func formatPython(path string) bool {
	root, tools := pythonProject(path)
	if root != "" && (tools["black"] || tools["isort"] || tools["autoflake"]) && classicPythonChain(root, path, tools) {
		return true
	}
	// ruff-only chain: check --fix covers autoflake (F401/F841) + isort (I); format is black-compatible.
	fixed := run(root, "ruff", "check", "--fix", "--quiet", path)
	return run(root, "ruff", "format", "--quiet", path) || fixed
}

// classicPythonChain runs ruff check --fix, autoflake, isort, black — each only when the project declares
// it, each from the project dir so the tool reads that pyproject. Reports whether any step ran.
func classicPythonChain(root, path string, tools map[string]bool) bool {
	ran := false
	if tools["ruff"] {
		ran = run(root, "ruff", "check", "--fix", "--quiet", path) || ran
	}
	if tools["autoflake"] {
		// --in-place explicitly: without the project's [tool.autoflake] in reach autoflake only prints.
		ran = run(root, "autoflake", "--in-place", "--quiet", path) || ran
	}
	if tools["isort"] {
		ran = run(root, "isort", "--quiet", path) || ran
	}
	if tools["black"] {
		ran = run(root, "black", "--quiet", path) || ran
	}
	return ran
}

// pythonProject walks up to the nearest pyproject.toml and reports its dir plus the [tool.*] sections it
// declares; "" when there is none.
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
	for _, m := range toolSection.FindAllSubmatch(body, -1) {
		tools[string(m[1])] = true
	}
	return root, tools
}

// resolveTool prefers the project's .venv, then PATH; "" when absent.
func resolveTool(root, name string) string {
	if root != "" {
		if p := filepath.Join(root, ".venv", "bin", name); isExec(p) {
			return p
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
	return cmd.Run() == nil
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// nearestDirWith returns the closest ancestor (within 8 levels) of path holding name.
func nearestDirWith(path, name string) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return ""
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
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
