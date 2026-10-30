package mutate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestApply(t *testing.T) {
	got, err := Apply("a+b", "+", "-")
	if err != nil || got != "a-b" {
		t.Fatalf("Apply = %q, %v", got, err)
	}
	for name, old := range map[string]string{"absent": "zzz", "twice": "a", "empty": ""} {
		src := "a+b a"
		if _, err := Apply(src, old, "x"); !errors.Is(err, ErrNotOnce) {
			t.Errorf("%s: err = %v, want ErrNotOnce", name, err)
		}
	}
}

func exitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("setup: %v", err)
	}
	return err
}

func TestClassify(t *testing.T) {
	ee := exitErr(t)
	cases := []struct {
		name string
		out  string
		err  error
		v    Verdict
		pkg  string
	}{
		{"pass", "ok\tp\t0.1s\n", nil, Vacuous, ""},
		{"fail line", "--- FAIL: T\nFAIL\texample.com/p\t0.1s\n", ee, Teeth, "example.com/p"},
		{"fail no pkg line", "--- FAIL: T\n", ee, Teeth, ""},
		{"panic", "panic: boom\n", ee, Teeth, ""},
		{"build failed", "FAIL\tp [build failed]\n", ee, Invalid, ""},
		{"setup failed", "FAIL\tp [setup failed]\n", ee, Invalid, ""},
		{"non-exit error", "", errors.New("no go"), Invalid, ""},
		{"exit without signature", "weird\n", ee, Invalid, ""},
	}
	for _, c := range cases {
		v, pkg := Classify(c.out, c.err)
		if v != c.v || pkg != c.pkg {
			t.Errorf("%s: got (%v,%q), want (%v,%q)", c.name, v, pkg, c.v, c.pkg)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o, code, done := ParseArgs([]string{"f.go", "old", "new", "--run", "TestX", "./p", "./q"})
	if done || code != 0 || o.File != "f.go" || o.Old != "old" || o.New != "new" || o.Run != "TestX" ||
		len(o.Pkgs) != 2 || o.Pkgs[0] != "./p" {
		t.Fatalf("got %+v code=%d done=%v", o, code, done)
	}
	if o, _, _ = ParseArgs([]string{"--run", "R", "f", "a", "b", "./p"}); o.Run != "R" || o.File != "f" {
		t.Errorf("leading --run: %+v", o)
	}
	if _, code, done = ParseArgs([]string{"f", "a", "b"}); !done || code != ExitUsage {
		t.Errorf("too few: code=%d done=%v", code, done)
	}
	if _, code, done = ParseArgs([]string{"-h"}); !done || code != 0 {
		t.Errorf("-h: code=%d done=%v", code, done)
	}
}

const modSrc = "package m\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Unused() int { return 1 }\n"

const modTest = `package m

import "testing"

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fatal("bad add")
	}
}

func TestUnused(t *testing.T) { _ = Unused() }
`

var (
	modOnce sync.Once
	modDir  string
	modErr  error
)

// sharedModule builds one temp module reused by every Check case.
func sharedModule(t *testing.T) (dir, file string) {
	t.Helper()
	modOnce.Do(func() {
		modDir, modErr = os.MkdirTemp("", "mutate-mod-")
		if modErr != nil {
			return
		}
		for name, body := range map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.21\n", "m.go": modSrc, "m_test.go": modTest,
		} {
			if modErr = os.WriteFile(filepath.Join(modDir, name), []byte(body), 0o644); modErr != nil {
				return
			}
		}
	})
	if modErr != nil {
		t.Fatal(modErr)
	}
	return modDir, filepath.Join(modDir, "m.go")
}

func TestMain(m *testing.M) {
	code := m.Run()
	if modDir != "" {
		os.RemoveAll(modDir)
	}
	os.Exit(code)
}

func TestCheck(t *testing.T) {
	dir, file := sharedModule(t)
	orig, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, old, new, run string
		want                Verdict
	}{
		{"teeth", "a + b", "a - b", "TestAdd", Teeth},
		{"vacuous", "return 1", "return 2", "TestUnused", Vacuous},
		{"compile error", "a + b", "a + undefinedSym", "", Invalid},
	}
	for _, c := range cases {
		r, err := Check(Options{File: file, Old: c.old, New: c.new, Run: c.run, Dir: dir, Pkgs: []string{"./..."}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if r.Verdict != c.want {
			t.Errorf("%s: verdict %v, want %v\n%s", c.name, r.Verdict, c.want, r.Output)
		}
		if c.want == Teeth && r.Pkg != "example.com/m" {
			t.Errorf("%s: pkg %q", c.name, r.Pkg)
		}
		now, _ := os.ReadFile(file)
		if !bytes.Equal(now, orig) {
			t.Fatalf("%s: source file changed", c.name)
		}
	}
	// Apply errors surface as (Invalid, err).
	for _, old := range []string{"nope-absent", "int"} {
		r, err := Check(Options{File: file, Old: old, New: "x", Dir: dir, Pkgs: []string{"./..."}})
		if !errors.Is(err, ErrNotOnce) || r.Verdict != Invalid {
			t.Errorf("old %q: (%v, %v)", old, r.Verdict, err)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	dir, file := sharedModule(t)
	t.Chdir(dir)
	quiet(t)
	if got := Run([]string{file, "a + b", "a - b", "--run", "TestAdd", "./..."}); got != ExitTeeth {
		t.Errorf("teeth exit %d", got)
	}
	if got := Run([]string{file, "return 1", "return 2", "--run", "TestUnused", "./..."}); got != ExitVacuous {
		t.Errorf("vacuous exit %d", got)
	}
	if got := Run([]string{file, "a + b", "a + undefinedSym", "./..."}); got != ExitUsage {
		t.Errorf("compile error exit %d", got)
	}
	if got := Run([]string{file, "absent-text", "x", "./..."}); got != ExitUsage {
		t.Errorf("absent exit %d", got)
	}
}

// quiet sends stdout/stderr to /dev/null so nested "[build failed]" text cannot leak into an outer mutate run.
func quiet(t *testing.T) {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	so, se := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = null, null
	t.Cleanup(func() { os.Stdout, os.Stderr = so, se; null.Close() })
}
