// Package helptext prints a verb's usage text in the `claude-code -h` palette: UPPERCASE heads dim and bold,
// the command/flag column bold along the status line's gradient, everything else untouched.
package helptext

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// Page is a verb's help: synopsis, blurb, command rows, flags and an optional trailing hint.
type Page struct {
	Usage, About string
	Commands     [][2]string
	Flags        *flag.FlagSet
	Hint         string
}

// Fprint renders p to w in the shared help layout, painted on a terminal.
func (p Page) Fprint(w io.Writer) {
	var secs []string
	if p.Usage != "" {
		secs = append(secs, "usage: "+strings.ReplaceAll(p.Usage, "\n", "\n       "))
	}
	if p.About != "" {
		var about []string
		for para := range strings.SplitSeq(p.About, "\n") {
			about = append(about, WrapRow("", para, 0, 80)...)
		}
		secs = append(secs, strings.Join(about, "\n"))
	}
	flags := flagRows(p.Flags)
	lw := 0
	for _, r := range append(append([][2]string{}, p.Commands...), flags...) {
		lw = max(lw, utf8.RuneCountInString(r[0])+4)
	}
	lw = min(lw, 30)
	if len(p.Commands) > 0 {
		secs = append(secs, "COMMANDS\n"+rowsText(p.Commands, lw))
	}
	if len(flags) > 0 {
		secs = append(secs, "FLAGS\n"+rowsText(flags, lw))
	}
	if p.Hint != "" {
		secs = append(secs, strings.TrimSuffix(p.Hint, "\n"))
	}
	Fprint(w, strings.Join(secs, "\n\n")+"\n")
}

// rowsText lays out rows under a shared label column, keeping 2+ spaces after an over-wide label.
func rowsText(rows [][2]string, lw int) string {
	var lines []string
	for _, r := range rows {
		label := "  " + r[0]
		w := max(lw, utf8.RuneCountInString(label)+2)
		lines = append(lines, WrapRow(label, r[1], w, 80)...)
	}
	return strings.Join(lines, "\n")
}

// Placeholder is an optional flag.Value hook: a custom Value implementing it names its own FLAGS
// placeholder (e.g. "KEY") instead of the generic "value" flag.UnquoteUsage falls back to. Backticks
// are banned in usage strings, so this is the only way to override the placeholder.
type Placeholder interface {
	Placeholder() string
}

// shorthandPrefix marks a one-letter alias; its row folds into the long flag's as "-p, --profile NAME".
const shorthandPrefix = "shorthand for --"

// flagRows turns fs's flags into one {label, description} row each, in VisitAll order.
func flagRows(fs *flag.FlagSet) [][2]string {
	if fs == nil {
		return nil
	}
	short := map[string]string{} // long name -> its one-letter alias
	fs.VisitAll(func(f *flag.Flag) {
		if long, ok := strings.CutPrefix(f.Usage, shorthandPrefix); ok && len(f.Name) == 1 && fs.Lookup(long) != nil {
			short[long] = f.Name
		}
	})
	var rows [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		if long, ok := strings.CutPrefix(f.Usage, shorthandPrefix); ok && short[long] == f.Name {
			return
		}
		name, usage := flag.UnquoteUsage(f)
		if p, ok := f.Value.(Placeholder); ok {
			name = p.Placeholder()
		}
		label := "--" + f.Name
		if len(f.Name) == 1 {
			label = "-" + f.Name
		}
		if s := short[f.Name]; s != "" {
			label = "-" + s + ", " + label
		}
		if name != "" {
			label += " " + name
		}
		switch f.DefValue {
		case "", "0", "false", "0s", "[]":
		default:
			if !strings.Contains(usage, "(default") {
				if fmt.Sprintf("%T", f.Value) == "*flag.stringValue" {
					usage += fmt.Sprintf(" (default %q)", f.DefValue)
				} else {
					usage += fmt.Sprintf(" (default %v)", f.DefValue)
				}
			}
		}
		rows = append(rows, [2]string{label, usage})
	})
	return rows
}

// WantsHelp reports a -h/-help/--help token before any literal "--" terminator, so a caller can route
// explicit help to stdout itself instead of letting fs.Parse's automatic usage call land on stderr.
func WantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "-help" || a == "--help" {
			return true
		}
	}
	return false
}

// ExitCode maps a FlagSet parse error to the verb's exit code: 0 after -h, 2 for a bad flag.
func ExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// Parse parses args into fs: (0,false) to continue, (0,true) after -h, (2,true) on a bad flag.
func Parse(fs *flag.FlagSet, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		return ExitCode(err), true
	}
	return 0, false
}

// NewFlagSet builds a ContinueOnError FlagSet on stderr whose -h renders p with p.Flags set to it.
func NewFlagSet(name string, p Page) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		p.Flags = fs
		p.Fprint(fs.Output())
	}
	return fs
}

// isRow: an indented label with a description column, or a bare flag line (`  -name type`) from PrintDefaults.
func isRow(l string) bool {
	if isTableHead(l) {
		return false
	}
	m := row.FindStringSubmatch(l)
	if m == nil || len(m[1]) > 4 {
		return false
	}
	return m[3] != "" || strings.HasPrefix(m[2], "-")
}

// isTableHead: an indented column-header line, two or more all-caps columns (PROVIDER  PROTOCOL  AUTH).
func isTableHead(l string) bool {
	m := row.FindStringSubmatch(l)
	if m == nil || m[3] == "" || len(m[1]) > 4 {
		return false
	}
	t := strings.TrimSpace(l)
	return strings.ToUpper(t) == t && strings.ContainsFunc(t, func(r rune) bool { return r >= 'A' && r <= 'Z' })
}

// isHead: an unindented all-caps section title (FLAGS, EXAMPLES:), never a usage line.
func isHead(l string) bool {
	if isTableHead(l) {
		return true
	}
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
	for w := range strings.FieldsSeq(desc) {
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

// DocsFlag is the flag every wrapped-CLI verb takes to print the wrapped binary's own help.
const DocsFlag = "--docs"

// WrappedDocs runs `bin --help` with the terminal attached, so a wrapped CLI's own documentation is one flag away;
// the exit code is the binary's.
func WrappedDocs(bin string) int {
	path, err := exec.LookPath(bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: %s is not on PATH; install it to read its docs\n", bin)
		return 1
	}
	cmd := exec.Command(path, "--help") //nolint:noctx // interactive help; nothing to cancel
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	var ee *exec.ExitError
	if err := cmd.Run(); errors.As(err, &ee) {
		return ee.ExitCode()
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: %s --help: %v\n", bin, err)
		return 1
	}
	return 0
}
