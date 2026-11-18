package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func BenchmarkCommentViolations(b *testing.B) {
	for _, prose := range []string{"// One line.\n", "// First line.\n// Second line.\n"} {
		b.Run(strings.TrimSpace(prose), func(b *testing.B) {
			content := "package fixture\n" + strings.Repeat("\n"+prose+"func F() {}\n", 200)
			spans := lineSpans{{1, 1 << 30}}
			b.ReportAllocs()
			for b.Loop() {
				commentViolations(content, content, spans)
			}
		})
	}
}

func TestTestLayoutViolations(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"ok.go", "bad.go", "plat_unix.go", "other_windows.go", "bar_x.go", "lin_linux.go", "win_windows_amd64.go", "uni_unix.go", "oth_other.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, text, base string
		want             int
		renamed          bool
	}{
		{"ok_test.go", "package x\n", "", 0, false},
		{"ok_regression_test.go", "package x\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n", "", 0, false},
		{"ok_regression_test.go", "package x\n\nfunc TestA(t *testing.T) {}\n", "", 1, false},
		{"ok_regression_test.go", "package x\n\nfunc TestA(t *testing.T) {}\n", "package x\n\nfunc TestA(t *testing.T) {}\n", 0, false},
		{"plat_test.go", "package x\n", "", 0, false},
		{"bar_test.go", "package x\n", "", 1, false},
		{"lin_test.go", "package x\n", "", 0, false},
		{"win_test.go", "package x\n", "", 0, false},
		{"uni_test.go", "package x\n", "", 0, false},
		{"oth_test.go", "package x\n", "", 0, false},
		{"other_test.go", "package x\n", "", 0, false},
		{"plat_other_test.go", "package x\n", "", 1, false},
		{"helpers_test.go", "package x\n", "package x\n", 1, true},
		{"ok_test.go", "package x\n", "package x\n", 0, true},
		{"helpers_test.go", "package x\n", "", 1, false},
		{"bad_test.go", "package x_test\n", "", 1, false},
		{"bad_regression_test.go", "//go:build linux\n\npackage x\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n", "", 1, false},
		{"old_test.go", "package x_test\n", "package x_test\n", 0, false},
		{"bad_test.go", "package bar_test\n", "package foo_test\n", 0, false},
		{"ok_test.go", "package foo_test\n", "package x\n", 1, false},
		{"ok_test.go", "//go:building examples\n\npackage x\n", "", 0, false},
		{"ok_test.go", "// +buildx\n\npackage x\n", "", 0, false},
		{"ok_test.go", "//go:build linux\n\npackage x\n", "", 1, false},
		{"ok_test.go", "// +build linux\n\npackage x\n", "", 1, false},
		{"internal/memory/jobscope_test.go", "package x_test\n", "", 0, false},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.name)
		if got := testLayoutViolations(path, c.text, c.base, c.renamed); len(got) != c.want {
			t.Errorf("%s: %v, want %d finding(s)", c.name, got, c.want)
		}
	}
}

func TestRunSimplifyFailBatchedGitAnswers(t *testing.T) {
	dir := gitFixture(t)
	stack := "\n// one\n// two\nfunc A() {}\n"
	for _, f := range []string{"old.go", "gone.go", "sp ace.go", "far.go"} {
		hosttest.WriteRel(t, dir, f, longGo())
	}
	hosttest.In(t, dir, "add", ".")
	hosttest.In(t, dir, "commit", "-qm", "base")
	hosttest.In(t, dir, "mv", "old.go", "new.go")
	hosttest.In(t, dir, "rm", "-q", "gone.go")
	hosttest.WriteRel(t, dir, "new.go", longGo()+stack)
	hosttest.WriteRel(t, dir, "sp ace.go", longGo()+stack)
	hosttest.WriteRel(t, dir, "fresh.go", "package fx\n"+stack)
	code, out := runSimplifyFail(t)
	want := "fresh.go:3: Comment block has 2 lines (max: 1)\nnew.go:74: Comment block has 2 lines (max: 1)\nsp ace.go:74: Comment block has 2 lines (max: 1)\n"
	if code != 1 || !strings.HasPrefix(out, want) || !strings.Contains(out, "3 new violation(s)") {
		t.Fatalf("code=%d, want only the added blocks of the renamed, spaced and untracked files:\n%s", code, out)
	}
	hosttest.WriteRel(t, dir, "bin.go", "package fx\n\x00\x01\n")
	if code, out = runSimplifyFail(t); code != 1 || !strings.Contains(out, "bin.go: go syntax error") {
		t.Fatalf("a binary .go file must fail the run (code %d):\n%s", code, out)
	}
}
