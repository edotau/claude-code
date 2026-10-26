package mutate

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
)

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
