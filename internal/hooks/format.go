package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// toolTimeout bounds one formatter run.
const toolTimeout = 10 * time.Second

// formatFile formats one file by extension with whichever formatter is installed; reports whether one ran cleanly.
func formatFile(path string) bool {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return false
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

var toolSection = regexp.MustCompile(`(?m)^\s*\[tool\.([A-Za-z0-9_-]+)[.\]]`)

// formatPython runs the classic chain (isort → black) when the nearest pyproject declares it, else ruff.
func formatPython(path string) bool {
	if root := nearestDirWith(path, "pyproject.toml"); root != "" {
		body, _ := os.ReadFile(filepath.Join(root, "pyproject.toml"))
		tools := map[string]bool{}
		for _, m := range toolSection.FindAllSubmatch(body, -1) {
			tools[string(m[1])] = true
		}
		if !tools["ruff"] && (tools["black"] || tools["isort"]) {
			ran := tools["isort"] && run(root, "isort", "--quiet", path)
			return tools["black"] && run(root, "black", "--quiet", path) || ran
		}
		fixed := run(root, "ruff", "check", "--fix", "--quiet", path)
		return run(root, "ruff", "format", "--quiet", path) || fixed
	}
	fixed := run("", "ruff", "check", "--fix", "--quiet", path)
	return run("", "ruff", "format", "--quiet", path) || fixed
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
