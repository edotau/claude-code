package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// Menu layout widths.
const (
	menuWidth = 80
	nameWidth = 16
)

// menuHeader is the menu's fixed opening; it opens on `usage:` — no banner.
var menuHeader = []string{
	"usage: claude-code [<command>] [subcommand] [flags]",
	"       claude-code <command> -h    per-command help",
	"       claude-code --host <h> ...  run a command on a tailnet host",
}

// menuRow is one line after the header: blank, a group head, or a verb (name + summary at gradient position frac).
type menuRow struct {
	head, name, summary string
	frac                float64
}

// visibleGroups returns the visible verbs' group names in declaration order and the visible verb count.
func visibleGroups(cmds []command) ([]string, int) {
	var groups []string
	seen := map[string]bool{}
	total := 0
	for _, c := range cmds {
		if c.hidden {
			continue
		}
		total++
		if !seen[c.group] {
			seen[c.group] = true
			groups = append(groups, c.group)
		}
	}
	return groups, total
}

// menuRows lays out the visible verbs: UPPERCASE groups in declaration order, each preceded by a blank line.
func menuRows() []menuRow {
	cmds := commands()
	groups, total := visibleGroups(cmds)
	var rows []menuRow
	n := 0
	for _, g := range groups {
		rows = append(rows, menuRow{}, menuRow{head: g})
		var group []command
		for _, c := range cmds {
			if !c.hidden && c.group == g {
				group = append(group, c)
			}
		}
		if alphabetical[g] {
			slices.SortStableFunc(group, func(a, b command) int { return strings.Compare(a.name, b.name) })
		}
		for _, c := range group {
			rows = append(rows, menuRow{name: fmt.Sprintf("%-*s", nameWidth, c.name), summary: c.summary,
				frac: float64(n) / float64(max(total-1, 1))})
			n++
		}
	}
	return rows
}

// usage renders the grouped menu, painting heads dim and the name column along the status line's gradient.
// It buffers the whole menu and returns the single write's error so a closed or broken stdout fails the command.
func usage(w *os.File) error {
	var b strings.Builder
	for _, l := range menuHeader {
		fmt.Fprintln(&b, l)
	}
	paintHead := func(s string) string { return s }
	paintName := func(s string, frac float64) string { return s }
	if ansi.Enabled(w) {
		mode, stops, dim := theme.Theme()
		paintHead = func(s string) string { return ansi.Paint(mode, dim, s, true) }
		paintName = func(s string, frac float64) string { return ansi.Paint(mode, ansi.GradAt(stops, frac), s, true) }
	}
	for _, r := range menuRows() {
		switch {
		case r.head != "":
			fmt.Fprintln(&b, paintHead(r.head))
		case r.name != "":
			lines := helptext.Row("  "+r.name, r.summary, 2+nameWidth, helptext.TermWidth(w))
			fmt.Fprintf(&b, "  %s%s\n", paintName(r.name, r.frac), lines[0][min(2+nameWidth, len(lines[0])):])
			for _, l := range lines[1:] {
				fmt.Fprintln(&b, l)
			}
		default:
			fmt.Fprintln(&b)
		}
	}
	fmt.Fprintln(&b) // Why: a blank line between the menu and the next shell prompt
	_, err := io.WriteString(w, b.String())
	return err
}
