package mutate

import (
	"github.com/edotau/claude-code/internal/hosttest"

	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
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
	cases := []struct {
		name                string
		args                []string
		wantDone            bool
		wantCode            int
		file, old, new, run string
		pkgs                []string
	}{
		{"full", []string{"f.go", "old", "new", "--run", "TestX", "./p", "./q"}, false, 0, "f.go", "old", "new", "TestX", []string{"./p", "./q"}},
		{"leading --run", []string{"--run", "R", "f", "a", "b", "./p"}, false, 0, "f", "a", "b", "R", []string{"./p"}},
		{"too few", []string{"f", "a", "b"}, true, ExitUsage, "", "", "", "", nil},
		{"-h", []string{"-h"}, true, 0, "", "", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, code, done := ParseArgs(c.args)
			if done != c.wantDone || code != c.wantCode {
				t.Fatalf("code=%d done=%v, want code=%d done=%v", code, done, c.wantCode, c.wantDone)
			}
			if c.wantDone {
				return
			}
			if o.File != c.file || o.Old != c.old || o.New != c.new || o.Run != c.run || !slices.Equal(o.Pkgs, c.pkgs) {
				t.Errorf("got %+v", o)
			}
		})
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
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	if modDir != "" {
		os.RemoveAll(modDir)
	}
	os.Exit(code)
}

func TestGoTestTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\necho fetching-dependencies\nexec sleep 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := goTestTimeout
	goTestTimeout = 500 * time.Millisecond
	t.Cleanup(func() { goTestTimeout = original })
	for _, tc := range []struct{ name, overlay string }{
		{"baseline", ""},
		{"mutated", filepath.Join(dir, "overlay.json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			out, err := goTest(Options{Dir: dir, Pkgs: []string{"."}}, tc.overlay)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("goTest let the dependency fetch finish instead of cancelling it")
			}
			if out != "fetching-dependencies\n" {
				t.Fatalf("goTest did not reach the simulated dependency fetch: output %q, error %v", out, err)
			}
			if elapsed >= 2*time.Second {
				t.Fatalf("goTest took %v, want cancellation before the 3-second dependency fetch finishes", elapsed)
			}
		})
	}
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
		t.Run(c.name, func(t *testing.T) {
			t.Parallel() // Check only reads the shared module; each nested go test gets its own overlay dir
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
		})
	}
}

// Apply errors surface as (Invalid, err).
func TestCheckApplyErrors(t *testing.T) {
	dir, file := sharedModule(t)
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
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"teeth", []string{file, "a + b", "a - b", "--run", "TestAdd", "./..."}, ExitTeeth},
		{"vacuous", []string{file, "return 1", "return 2", "--run", "TestUnused", "./..."}, ExitVacuous},
		{"compile error", []string{file, "a + b", "a + undefinedSym", "./..."}, ExitUsage},
		{"absent", []string{file, "absent-text", "x", "./..."}, ExitUsage},
	}
	var wg sync.WaitGroup
	for _, c := range cases {
		wg.Go(func() {
			if got := Run(c.args); got != c.want {
				t.Errorf("%s exit %d", c.name, got)
			}
		})
	}
	wg.Wait()
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

// A package that already fails without the mutation must not be reported as Teeth.
func TestCheckRedBaseline(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":    "module example.com/red\n\ngo 1.21\n",
		"m.go":      "package m\n\nfunc One() int { return 1 }\n",
		"m_test.go": "package m\n\nimport \"testing\"\n\nfunc TestRed(t *testing.T) { t.Fatal(\"already red\") }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Check(Options{File: filepath.Join(dir, "m.go"), Old: "return 1", New: "return 2", Dir: dir, Pkgs: []string{"./..."}})
	if err == nil || r.Verdict != Invalid {
		t.Fatalf("red baseline: verdict %v, err %v, want Invalid and an error", r.Verdict, err)
	}
}
