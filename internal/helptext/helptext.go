// Package helptext prints a verb's usage text in the `claude-code -h` palette: UPPERCASE heads dim and bold,
// the command/flag column bold along the status line's gradient, everything else untouched.
package helptext

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/statusline"
)

// row is an indented command line: the left column ends at the first run of 2+ spaces (or the line, for a flag).
var row = regexp.MustCompile(`^(\s{2,})(\S.*?)(\s{2,}\S.*)?$`)

// Fprint writes text to w, painted when w is a terminal and NO_COLOR is unset; otherwise byte-identical.
func Fprint(w io.Writer, text string) {
	if !ansi.Enabled(w) {
		fmt.Fprint(w, text)
		return
	}
	fmt.Fprint(w, Paint(text))
}

// Paint colors text unconditionally: heads in the dim slot, row labels across the gradient in row order.
func Paint(text string) string {
	mode, stops, dim := statusline.Theme()
	lines := strings.Split(text, "\n")
	var rows []int
	for i, l := range lines {
		if isRow(l) {
			rows = append(rows, i)
		}
	}
	for n, i := range rows {
		m := row.FindStringSubmatch(lines[i])
		frac := float64(n) / float64(max(len(rows)-1, 1))
		lines[i] = m[1] + ansi.Paint(mode, ansi.GradAt(stops, frac), m[2], true) + m[3]
	}
	for i, l := range lines {
		if isHead(l) {
			lines[i] = ansi.Paint(mode, dim, l, true)
		}
	}
	return strings.Join(lines, "\n")
}

// singleDash matches PrintDefaults' leading "-name" so it can be widened to "--name".
var singleDash = regexp.MustCompile(`(?m)^(\s+)-([^-\s])`)

// Defaults prints fs's flag defaults through Fprint, so `-h` on a FlagSet verb matches the menu.
func Defaults(fs *flag.FlagSet) {
	var b bytes.Buffer
	out := fs.Output()
	fs.SetOutput(&b)
	fs.PrintDefaults()
	fs.SetOutput(out)
	text := singleDash.ReplaceAllString(b.String(), "$1--$2")
	Fprint(out, text)
}

// NewFlagSet builds a FlagSet whose -h prints usage then Defaults, painted like the rest of the help palette.
func NewFlagSet(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		Fprint(fs.Output(), usage)
		Defaults(fs)
	}
	return fs
}

// isRow: an indented label with a description column, or a bare flag line (`  -name type`) from PrintDefaults.
func isRow(l string) bool {
	m := row.FindStringSubmatch(l)
	if m == nil || len(m[1]) > 4 {
		return false
	}
	return m[3] != "" || strings.HasPrefix(m[2], "-")
}

// isHead: an unindented all-caps section title (FLAGS, EXAMPLES:), never a usage line.
func isHead(l string) bool {
	t := strings.TrimSuffix(strings.TrimSpace(l), ":")
	return t != "" && l[0] != ' ' && strings.ToUpper(t) == t && strings.ContainsFunc(t, func(r rune) bool { return r >= 'A' && r <= 'Z' })
}

// WrapRow lays out `label` padded to labelWidth plus desc, wrapping desc at word boundaries within width columns;
// continuation lines are indented to the description column. A word longer than the space overflows its line.
func WrapRow(label, desc string, labelWidth, width int) []string {
	pad := max(labelWidth-utf8.RuneCountInString(label), 0)
	first := label + strings.Repeat(" ", pad)
	col := utf8.RuneCountInString(first)
	var lines []string
	cur := first
	empty := true
	for _, w := range strings.Fields(desc) {
		if !empty && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(w) > width {
			lines = append(lines, cur)
			cur, empty = strings.Repeat(" ", col), true
		}
		if !empty {
			cur += " "
		}
		cur += w
		empty = false
	}
	return append(lines, cur)
}
