package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScopedVerifyGateRequiresExactCheckVariable(t *testing.T) {
	for _, c := range []struct {
		name, variable string
		wantLint       bool
	}{
		{"make CHECKER", "$(CHECKER)", false},
		{"braced CHECKER", "${CHECKER}", false},
		{"shell CHECKER", "$$CHECKER", false},
		{"make CHECK", "$(CHECK)", true},
		{"braced CHECK", "${CHECK}", true},
		{"shell CHECK", "$$CHECK", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, "go.mod")
			body := "lint:\n\techo " + c.variable + "\n"
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			want := fullGoGate
			if c.wantLint {
				want = "make lint CHECK=1 && " + want
			}
			if gate, source := ScopedVerifyGate(dir, nil, true); gate != want || source != "go.mod" {
				t.Fatalf("CHECK reference %q: gate=%q source=%q, want %q from go.mod", c.variable, gate, source, want)
			}
		})
	}
}

func TestScopedVerifyGatePythonOptionLikePath(t *testing.T) {
	for _, c := range []struct {
		name, files, want string
	}{
		{"pytest", "pyproject.toml", "pytest -q './--collect-only'"},
		{"ruff and pytest", "pyproject.toml,ruff.toml", "ruff check './--collect-only' && pytest -q './--collect-only'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, c.files)
			gate, source := ScopedVerifyGate(dir, []string{"./--collect-only"}, false)
			if gate != c.want || source != "pyproject" {
				t.Fatalf("option-like scope must stay a path: gate=%q source=%q, want %q from pyproject", gate, source, c.want)
			}
		})
	}
}

func TestScopedVerifyGateNpmAndCargoScope(t *testing.T) {
	for _, c := range []struct {
		name, marker, body string
		scope              []string
		want, source       string
	}{
		{"npm whole suite", "package.json", `{"scripts":{"test":"vitest"}}`, nil, "npm test", "package.json"},
		{"npm scoped", "package.json", `{"scripts":{"test":"vitest"}}`, []string{"./tests/my pkg/", "other"},
			"npm test -- 'tests/my pkg' 'other'", "package.json"},
		{"cargo whole suite", "Cargo.toml", "[package]\nname = 'example'\n", nil, "cargo build && cargo test", "Cargo.toml"},
		{"cargo scoped refuses", "Cargo.toml", "[package]\nname = 'example'\n", []string{"src"}, "", "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.marker), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			gate, source := ScopedVerifyGate(dir, c.scope, false)
			if gate != c.want || source != c.source {
				t.Fatalf("scope=%q: gate=%q source=%q, want %q from %q", c.scope, gate, source, c.want, c.source)
			}
		})
	}
}
