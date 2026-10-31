// Package planlint checks plans and docs: assumption lint, goal verification, doc drift, workflow skeletons.
package planlint

import (
	"fmt"
	"io"
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

// readInput mirrors the shared "-" (stdin) or path.exists() gate in both scripts, exit 0 on miss.
func readInput(input string, stdin io.Reader, stderr io.Writer) (text, source string, ok bool) {
	if input == "-" {
		b, _ := io.ReadAll(stdin)
		return string(b), "stdin", true
	}
	b, err := os.ReadFile(input)
	if err != nil {
		fmt.Fprintf(stderr, "[error] %s not found\n", input)
		return "", "", false
	}
	return string(b), input, true
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
