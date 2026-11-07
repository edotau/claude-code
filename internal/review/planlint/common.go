// Package planlint checks plans and docs: assumption lint, goal verification, doc drift, workflow skeletons.
package planlint

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sync"

	"github.com/edotau/claude-code/internal/ui/helptext"
)

var lineSplitRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\r\n|\r|\n`) })

// splitLines mirrors Python's str.splitlines() for the line endings these scripts see in practice.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return lineSplitRe().Split(text, -1)
}

// readInput reads "-" (stdin) or a path; a missing path or any read failure exits 1 so a gate never passes unlinted.
func readInput(input string, stdin io.Reader, stderr io.Writer) (text, source string, exit int, ok bool) {
	if input == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "[error] reading stdin: %v\n", err)
			return "", "", 1, false
		}
		return string(b), "stdin", 0, true
	}
	b, err := os.ReadFile(input)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "[error] %s not found\n", input)
		return "", "", 1, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "[error] reading %s: %v\n", input, err)
		return "", "", 1, false
	}
	return string(b), input, 0, true
}

// parseInputJSONArgs parses the `review <tool> [<input>] [--json]` shape assumption_linter.py and
// goal_verifier.py's Go ports share, with a real FlagSet so -h prints the usage page anywhere in args.
// Explicit -h prints to stdout and exits 0; a bad flag stays on stderr, exit 2.
func parseInputJSONArgs(tool, about string, args []string, stdout, stderr io.Writer) (input string, jsonOut bool, exitCode int, handled bool) {
	fs := helptext.NewFlagSet("review "+tool, helptext.Page{
		Usage: "claude-code review " + tool + " [<input>] [flags]",
		About: about + " <input> is a file, or - (the default) for stdin.",
	})
	jsonFlag := fs.Bool("json", false, "Output in JSON format")
	if helptext.WantsHelp(args) {
		fs.SetOutput(stdout)
		fs.Usage()
		return "-", false, 0, true
	}
	fs.SetOutput(stderr)
	positional, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return "-", false, helptext.ExitCode(err), true
	}
	input = "-"
	switch len(positional) {
	case 0:
	case 1:
		input = positional[0]
	default:
		fmt.Fprintf(stderr, "review %s: unexpected argument %q\n", tool, positional[1])
		fs.Usage()
		return "-", false, 2, true
	}
	return input, *jsonFlag, 0, false
}

// pyRound1 replicates Python's round(x, 1) round-half-to-even at the tenths place.
func pyRound1(x float64) float64 {
	scaled := x * 10
	floor := float64(int64(scaled))
	if scaled < 0 && scaled != floor {
		floor -= 1
	}
	diff := scaled - floor
	var rounded float64
	switch {
	case diff < 0.5:
		rounded = floor
	case diff > 0.5:
		rounded = floor + 1
	default:
		if int64(floor)%2 == 0 {
			rounded = floor
		} else {
			rounded = floor + 1
		}
	}
	return rounded / 10
}

// formatPyFloat renders a float the way Python's f-string/json.dumps would (always keep one decimal here).
func formatPyFloat(v float64) string {
	return fmt.Sprintf("%.1f", v)
}
