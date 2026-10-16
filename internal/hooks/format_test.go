package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeRecipeAndTargetLine(t *testing.T) {
	body := "FMT := gofmt\nformat := no\n\n.PHONY: format\nformat: deps ## help\n\t@echo one\n\n# a comment does not end the recipe\n\t$(FMT) -w $(FILE)\nX := 1\n\tnot a recipe line\nfmt:\n\tgo fmt ./...\n"
	if got := makeRecipe(body, "format"); got != "@echo one\n$(FMT) -w $(FILE)" {
		t.Errorf("format recipe = %q", got)
	}
	if got := makeRecipe(body, "fmt"); got != "go fmt ./..." {
		t.Errorf("fmt recipe = %q", got)
	}
	if makeRecipe(body, "lint") != "" {
		t.Error("an absent target must yield no recipe")
	}
	if isTargetLine("format := x", "format") || isTargetLine("formatter: x", "format") || !isTargetLine("format : deps", "format") {
		t.Error("isTargetLine")
	}
	if !recipeReads("x ${TARGET}", "TARGET") || recipeReads("x $(TARGETS)", "TARGET") {
		t.Error("recipeReads")
	}
}

func makeRepo(t *testing.T, makefile string) (root, mine, other string) {
	t.Helper()
	for _, tool := range []string{"make", "gofmt"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	root, _ = filepath.EvalSymlinks(t.TempDir())
	ugly := "package x\nfunc  F( ) {}\n"
	mine, other = filepath.Join(root, "mine.go"), filepath.Join(root, "other.go")
	for _, f := range []string{mine, other} {
		if err := os.WriteFile(f, []byte(ugly), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, mine, other
}

func formatted(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return !strings.Contains(string(b), "func  F")
}

// A per-file format target is the project's own formatter: it runs on the one file and nothing else.
func TestMakeFormatUsesRepoTarget(t *testing.T) {
	_, mine, other := makeRepo(t, ".PHONY: format\nformat:\n\tgofmt -w $(FILE)\n")
	if !formatFile(mine) {
		t.Fatal("formatFile must report the make target ran")
	}
	if !formatted(t, mine) || formatted(t, other) {
		t.Error("the target must format exactly the named file")
	}
}

// A whole-tree target reads no per-file variable: skipped, and the extension chain formats instead.
func TestMakeFormatSkipsWholeTreeTarget(t *testing.T) {
	root, mine, other := makeRepo(t, "format:\n\tgofmt -w .\n\ttouch ran-make\n")
	if makeFormat(mine) {
		t.Fatal("a target without a per-file variable must not run")
	}
	if _, err := os.Stat(filepath.Join(root, "ran-make")); err == nil {
		t.Error("the whole-tree target ran")
	}
	if !formatFile(mine) || !formatted(t, mine) || formatted(t, other) {
		t.Error("the extension chain must still format the one file")
	}
}

// A recipe that provisions an environment, or whose dry run never names the file, is never run for real.
func TestMakeFormatRefusesProvisioningAndUnreachedVariable(t *testing.T) {
	root, mine, _ := makeRepo(t, "format:\n\tpip install black && gofmt -w $(FILE)\n\ttouch ran-make\n")
	if makeFormat(mine) {
		t.Error("a provisioning recipe must not run")
	}
	if _, err := os.Stat(filepath.Join(root, "ran-make")); err == nil {
		t.Error("the provisioning target ran")
	}
	root, mine, _ = makeRepo(t, "format:\n\t$(if $(FILE),touch ran-make,)\n")
	if makeFormat(mine) {
		t.Error("a recipe that reads the variable but whose plan never names the file must not run")
	}
	if _, err := os.Stat(filepath.Join(root, "ran-make")); err == nil {
		t.Error("the unreached-variable target ran")
	}
}

// The declared python chain runs each declared tool from the project venv, in order; an undeclared
// project falls through to ruff.
func TestFormatPythonDeclaredChain(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	root := t.TempDir()
	bin := filepath.Join(root, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "trace")
	for _, tool := range []string{"ruff", "autoflake", "isort", "black"} {
		script := "#!/bin/sh\necho " + tool + " \"$@\" >> " + trace + "\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	py := filepath.Join(root, "pkg", "m.py")
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(py, []byte("x=1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[tool.black]\nline-length = 100\n[tool.autoflake]\nremove-all-unused-imports = true\n[tool.ruff.lint]\nselect = [\"E\"]\n"), 0o644)

	if !formatPython(py) {
		t.Fatal("the declared chain must report it ran")
	}
	got, _ := os.ReadFile(trace)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := []string{"ruff check --fix --quiet " + py, "autoflake --in-place --quiet " + py, "black --quiet " + py}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("chain ran %q, want %q", lines, want)
	}

	// No classic tool declared → the ruff chain, from the project dir.
	_ = os.Remove(trace)
	_ = os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname = \"x\"\n"), 0o644)
	if !formatPython(py) {
		t.Fatal("the ruff chain must report it ran")
	}
	got, _ = os.ReadFile(trace)
	if s := string(got); !strings.Contains(s, "ruff check --fix") || !strings.Contains(s, "ruff format --quiet") || strings.Contains(s, "black") {
		t.Errorf("ruff chain ran %q", s)
	}
}
