// Package plan ports assumption_linter.py, goal_verifier.py and workflow_skeleton.py to Go stdlib.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"
)

var lineSplitRe = regexp.MustCompile(`\r\n|\r|\n`)

// splitLines mirrors Python's str.splitlines() for the line endings these scripts see in practice.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return lineSplitRe.Split(text, -1)
}

// truncateRunes matches Python's s[:n] slicing, which counts code points, not bytes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
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

// parseInputJSONArgs accepts the argparse spelling (positional before or after --json) both scripts share.
func parseInputJSONArgs(prog string, args []string, stdout, stderr io.Writer) (input string, jsonOut bool, exitCode int, handled bool) {
	input = "-"
	seen := false
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case a == "-h" || a == "--help":
			fmt.Fprintf(stdout, "usage: %s [-h] [--json] [input]\n", prog)
			return input, jsonOut, 0, true
		case (strings.HasPrefix(a, "-") && a != "-") || seen:
			fmt.Fprintf(stderr, "%s: error: unrecognized arguments: %s\n", prog, a)
			return input, jsonOut, 2, true
		default:
			input, seen = a, true
		}
	}
	return input, jsonOut, 0, false
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

// marshalPyJSON matches Python's json.dumps(v, indent=2): ensure_ascii escapes every non-ASCII rune to
// \uXXXX (Go's stdlib doesn't), and HTML-escaping of <, >, & must be off (Python never escapes those).
func marshalPyJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")
	escaped := escapeNonASCII(compact)
	var out bytes.Buffer
	if err := json.Indent(&out, escaped, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func escapeNonASCII(b []byte) []byte {
	var out bytes.Buffer
	for _, r := range string(b) {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&out, "\\u%04x\\u%04x", r1, r2)
		} else {
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.Bytes()
}
