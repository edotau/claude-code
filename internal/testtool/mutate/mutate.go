// Package mutate checks that a test has teeth: it swaps one snippet in a source file via go's -overlay and reruns tests.
package mutate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/ui/helptext"
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
	if _, ok := errors.AsType[*exec.ExitError](runErr); !ok {
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

// source is the file under mutation: its resolved and given paths, contents, stat and hash before the run.
type source struct {
	abs, given string
	data       []byte
	info       os.FileInfo
	sum        [32]byte
}

// readSource resolves file and snapshots its contents, stat and hash.
func readSource(file string) (source, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return source{}, err
	}
	given := abs // go keys the overlay by the path it sees; on macOS /var vs /private/var both occur
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return source{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return source{}, err
	}
	return source{abs: abs, given: given, data: data, info: info, sum: sha256.Sum256(data)}, nil
}

// writeOverlay writes the mutated copy and the go overlay that swaps it in for src under tmp; returns the overlay path.
func writeOverlay(tmp string, src source, mutated string) (string, error) {
	copyPath := filepath.Join(tmp, filepath.Base(src.abs))
	if err := os.WriteFile(copyPath, []byte(mutated), 0o600); err != nil {
		return "", err
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {src.abs: copyPath, src.given: copyPath}})
	ovPath := filepath.Join(tmp, "overlay.json")
	if err := os.WriteFile(ovPath, ov, 0o600); err != nil {
		return "", err
	}
	return ovPath, nil
}

// goTestTimeout bounds one go test run, dependency fetch and compile included.
var goTestTimeout = 15 * time.Minute

// goTest runs go test over o.Pkgs (with the overlay unless ovPath is "") and returns its combined output and run error.
func goTest(o Options, ovPath string) (string, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, goTestTimeout)
	defer cancel()
	args := []string{"test", "-count=1"}
	if ovPath != "" {
		args = append(args, "-overlay", ovPath)
	}
	if o.Run != "" {
		args = append(args, "-run", o.Run)
	}
	cmd := exec.CommandContext(ctx, "go", append(args, o.Pkgs...)...)
	cmd.Dir = o.Dir
	cmd.WaitDelay = 10 * time.Second // a test binary orphaned by the kill must not hold the pipes open
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()
	return buf.String(), runErr
}

// Check mutates o.File through an overlay in a temp dir, runs go test, and never writes the tree.
func Check(o Options) (Result, error) {
	src, err := readSource(o.File)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	mutated, err := Apply(string(src.data), o.Old, o.New)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	// A red baseline would make any mutation look like teeth.
	if base, baseErr := goTest(o, ""); baseErr != nil {
		return Result{Verdict: Invalid, Output: base}, fmt.Errorf("baseline tests already fail without the mutation:\n%s", tail(base, 15))
	}
	tmp, err := os.MkdirTemp("", "cc-mutate-")
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	ovPath, err := writeOverlay(tmp, src, mutated)
	if err != nil {
		return Result{Verdict: Invalid}, err
	}
	out, runErr := goTest(o, ovPath)
	if err := assertUntouched(src.abs, src.info, src.sum); err != nil {
		return Result{Verdict: Invalid}, err
	}
	v, pkg := Classify(out, runErr)
	return Result{Verdict: v, Pkg: pkg, Output: out}, nil
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

// Page is the help for `claude-code test mutate`.
var Page = helptext.Page{
	Usage: "claude-code test mutate <file> <old> <new> [--run REGEX] <pkg...>",
	About: "Replace the one occurrence of <old> in <file> via go -overlay, rerun the\n" +
		"packages' tests, and say whether they notice. The tree is never written.",
	Hint: "Exit 0: teeth, the tests failed under the mutation.\n" +
		"Exit 1: vacuous, the tests passed with the mutation.\n" +
		"Exit 2: usage error, <old> not found once, or the mutant failed to build.",
}

// ParseArgs splits args into Options; --run may sit anywhere. code is nonzero (done) after -h or a usage error.
func ParseArgs(args []string) (o Options, code int, done bool) {
	fs := helptext.NewFlagSet("test mutate", Page)
	fs.StringVar(&o.Run, "run", "", "only run tests matching REGEX (go test -run)")
	var pos []string
	for len(args) > 0 {
		if c, d := helptext.Parse(fs, args); d {
			return o, c, true
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	if len(pos) < 4 {
		fmt.Fprintln(os.Stderr, "claude-code: test mutate: need <file> <old> <new> and at least one <pkg>")
		return o, ExitUsage, true
	}
	o.File, o.Old, o.New, o.Pkgs = pos[0], pos[1], pos[2], pos[3:]
	return o, 0, false
}

// Run is the `test mutate` entry: it prints a one-line verdict and returns the exit code.
func Run(args []string) int {
	o, code, done := ParseArgs(args)
	if done {
		return code
	}
	r, err := Check(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: test mutate: %v\n", err)
		return ExitUsage
	}
	switch r.Verdict {
	case Teeth:
		pkg := r.Pkg
		if pkg == "" {
			pkg = strings.Join(o.Pkgs, " ")
		}
		fmt.Printf("teeth: %s failed under the mutation\n", pkg)
		return ExitTeeth
	case Vacuous:
		fmt.Println("vacuous: tests passed with the mutation")
		return ExitVacuous
	}
	fmt.Fprintln(os.Stderr, "claude-code: test mutate: the mutant did not build or go test could not run")
	fmt.Fprint(os.Stderr, tail(r.Output, 15))
	return ExitUsage
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
