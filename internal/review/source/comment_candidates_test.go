package source

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestGoCommentCandidates(t *testing.T) {
	long := "// " + strings.Repeat("x", 125)
	cases := []struct{ name, src, kind, symbol string }{
		{"stacked", "package p\n\n// one\n// two\nfunc A() {}\n", "stacked", "A"},
		{"long", "package p\n\n" + long + "\nfunc A() {}\n", "long", "A"},
		{"restates func", "package p\n\n// getName gets the name.\nfunc getName() {}\n", "restates", "getName"},
		{"restates type", "package p\n\n// widget is a widget.\ntype widget struct{}\n", "restates", "widget"},
		{"restates field", "package p\n\ntype w struct {\n\t// size is the size.\n\tsize int\n}\n", "restates", "size"},
		{"trailing", "package p\n\nfunc A() {\n\tx := 1 // set x\n\t_ = x\n}\n", "trailing", "A"},
		{"informative doc", "package p\n\n// getName reads the vault once per process.\nfunc getName() {}\n", "", ""},
		{"exported doc", "package p\n\n// Name is the name.\nfunc Name() {}\n", "", ""},
		{"go directive", "package p\n\n//go:noinline\n//nolint:all\nfunc A() {}\n\nvar x = 1 //nolint:gosec\n", "", ""},
		{"exempt blocks", "package p\n\n// Gotcha: one\n// two\nfunc A() {}\n\n// Deprecated: one\n// two\nfunc B() {}\n\n// Regression (2026-01-01): a\n// b\nfunc C() {}\n", "", ""},
		{"file header", "// Package p does a\n// long thing.\npackage p\n", "", ""},
		{"output block", "package p\n\nfunc ExampleA() {\n\t// Output:\n\t// 1\n}\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := goCommentCandidates("p.go", c.src)
			if c.kind == "" {
				if len(got) != 0 {
					t.Fatalf("want none, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].kind != c.kind || got[0].symbol != c.symbol {
				t.Fatalf("want one %s on %s, got %+v", c.kind, c.symbol, got)
			}
		})
	}
}

func TestRunQualityCommentsTSV(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/p.go", []byte("package p\n\n// one\n// two\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := RunQuality([]string{"--comments", "--format", "tsv", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := dir + "/p.go:3\tstacked\tA\t2 lines: one\n"
	if out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	if code := RunQuality([]string{"--comments", dir}, &out, &errb); code != 2 {
		t.Fatalf("missing --format tsv: exit %d, want 2", code)
	}
}
