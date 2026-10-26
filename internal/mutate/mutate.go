// Package mutate checks that a test has teeth: it swaps one snippet in a source file via go's -overlay and reruns tests.
package mutate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Verdict is the outcome of one mutation run.
type Verdict int

const (
	Teeth   Verdict = iota // tests failed under the mutation
	Vacuous                // tests still passed
	Invalid                // the mutant did not compile or go test could not run
)

// Exit codes of the verb.
const (
	ExitTeeth   = 0
	ExitVacuous = 1
	ExitUsage   = 2
)

// ErrNotOnce is returned when the old snippet does not occur exactly once.
var ErrNotOnce = errors.New("old text must occur exactly once")

// Options describes one mutation run; Dir is the go test working directory ("" = cwd).
type Options struct {
	File, Old, New, Run, Dir string
	Pkgs                     []string
}

// Result is the verdict plus the failing package (Teeth) and go test's combined output.
type Result struct {
	Verdict Verdict
	Pkg     string
	Output  string
}

// Apply returns src with the single occurrence of old replaced by repl.
func Apply(src, old, repl string) (string, error) {
	if old == "" {
		return "", fmt.Errorf("%w: old text is empty", ErrNotOnce)
	}
	if n := strings.Count(src, old); n != 1 {
		return "", fmt.Errorf("%w: found %d occurrences", ErrNotOnce, n)
	}
	return strings.Replace(src, old, repl, 1), nil
}

var (
	failPkg   = regexp.MustCompile(`(?m)^FAIL[ \t]+(\S+)[ \t]`)
	buildFail = regexp.MustCompile(`(?m)^FAIL[ \t]+\S+ \[(build|setup) failed\]`) // go's own line, not test output
)

// Classify reads go test's output and run error: build/setup failures are Invalid, not Teeth.
func Classify(out string, runErr error) (Verdict, string) {
	if buildFail.MatchString(out) {
		return Invalid, ""
	}
	if runErr == nil {
		return Vacuous, ""
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		return Invalid, ""
	}
	if m := failPkg.FindStringSubmatch(out); m != nil {
		return Teeth, m[1]
	}
	if strings.Contains(out, "--- FAIL") || strings.Contains(out, "panic:") {
		return Teeth, ""
	}
	return Invalid, ""
}

// Check mutates o.File through an overlay in a temp dir, runs go test, and never writes the tree.
func Check(o Options) (Result, error) {
	abs, err := filepath.Abs(o.File)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	before, err := os.Stat(abs)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	sum := sha256.Sum256(src)
	mutated, err := Apply(string(src), o.Old, o.New)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	tmp, err := os.MkdirTemp("", "cc-mutate-")
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	defer os.RemoveAll(tmp)
	copyPath := filepath.Join(tmp, filepath.Base(abs))
	if err := os.WriteFile(copyPath, []byte(mutated), 0o600); err != nil {
		return Result{Verdict: Invalid}, err
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {abs: copyPath}})
	ovPath := filepath.Join(tmp, "overlay.json")
	if err := os.WriteFile(ovPath, ov, 0o600); err != nil {
		return Result{Verdict: Invalid}, err
	}
	args := []string{"test", "-overlay", ovPath, "-count=1"}
	if o.Run != "" {
		args = append(args, "-run", o.Run)
	}
	cmd := exec.Command("go", append(args, o.Pkgs...)...)
	cmd.Dir = o.Dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()
	if err := assertUntouched(abs, before, sum); err != nil {
		return Result{Verdict: Invalid}, err
	}
	v, pkg := Classify(buf.String(), runErr)
	return Result{Verdict: v, Pkg: pkg, Output: buf.String()}, nil
}

// assertUntouched fails if the source file's mtime or hash changed during the run.
func assertUntouched(path string, before os.FileInfo, sum [32]byte) error {
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !after.ModTime().Equal(before.ModTime()) || sha256.Sum256(b) != sum {
		return fmt.Errorf("%s changed during the mutation run", path)
	}
	return nil
}
