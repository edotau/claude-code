package cli

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// nameWidth is the menu's command-name column.
const nameWidth = 16

// menuHeader is the menu's fixed opening; it opens on `usage:` — no banner.
var menuHeader = []string{
	"usage: claude-code [<command>] [subcommand] [flags]",
	"       claude-code <command> -h      per-command help",
	"       claude-code --host <h> ...    run a command on a tailnet host",
	"       claude-code --remote <h> ...  Databricks requests leave via <h>",
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

// menuSVGPath is the README image; menuSVGPalette pins its gradient so the render never follows a local theme.
const (
	menuSVGPath    = "docs/images/cli-menu.svg"
	menuSVGPalette = "gemini"
)

// Terminal-window geometry: monospace cell width, line height, left margin, first baseline, bottom pad.
const (
	svgCell   = 8.4
	svgLine   = 19
	svgLeft   = 22.0
	svgTop    = 46
	svgBottom = 35
	svgFont   = "ui-monospace,SFMono-Regular,Menlo,Consolas,monospace"
	svgBG     = "#1d1a22"
	svgFG     = "#d7d4dc"
)

// menuSVG renders the menu as a dark terminal window: the same rows as usage, painted with the pinned palette.
func menuSVG() []byte {
	stops, dim := theme.EmbeddedTheme(menuSVGPalette)
	rows := menuRows()
	cols := 0
	for _, l := range menuHeader {
		cols = max(cols, utf8.RuneCountInString(l))
	}
	for _, r := range rows {
		cols = max(cols, utf8.RuneCountInString(r.head), 2+utf8.RuneCountInString(r.name+r.summary))
	}
	w := int(math.Round(2*svgLeft + float64(cols)*svgCell))
	h := svgTop + (len(menuHeader)+len(rows)-1)*svgLine + svgBottom
	dimHex := hexOf(dim)

	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="claude-code help menu">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" rx="10" fill="%s"/>`+"\n", w, h, svgBG)
	fmt.Fprintln(&b, `<circle cx="20" cy="18" r="6" fill="#ff5f57"/><circle cx="40" cy="18" r="6" fill="#febc2e"/><circle cx="60" cy="18" r="6" fill="#28c840"/>`)
	fmt.Fprintf(&b, `<text x="%.1f" y="22" fill="%s" font-family="%s" font-size="12" text-anchor="middle">$ claude-code</text>`+"\n", float64(w)/2, dimHex, svgFont)
	fmt.Fprintf(&b, `<g font-family="%s" font-size="14" xml:space="preserve">`+"\n", svgFont)
	text := func(col, line int, fill, s string, bold bool) {
		weight := ""
		if bold {
			weight = ` font-weight="bold"`
		}
		// NBSP keeps column alignment even where a renderer ignores xml:space.
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" fill="%s"%s>%s</text>`+"\n", svgLeft+float64(col)*svgCell,
			svgTop+line*svgLine, fill, weight, strings.ReplaceAll(html.EscapeString(s), " ", " "))
	}
	for i, l := range menuHeader {
		text(0, i, svgFG, l, false)
	}
	for i, r := range rows {
		line := len(menuHeader) + i
		switch {
		case r.head != "":
			text(0, line, dimHex, r.head, true)
		case r.name != "":
			text(0, line, svgFG, "  ", false)
			text(2, line, hexOf(ansi.GradAt(stops, r.frac)), r.name, true)
			text(2+utf8.RuneCountInString(r.name), line, svgFG, r.summary, false)
		}
	}
	b.WriteString("</g></svg>\n")
	return b.Bytes()
}

func hexOf(c ansi.RGB) string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

// cmdMenuSVG prints the menu SVG, or writes it with --out.
func cmdMenuSVG(args []string) int {
	fs := helptext.NewFlagSet("help-svg", helptext.Page{
		Usage: "claude-code help-svg [flags]",
		About: "Render the claude-code menu as the README's SVG; bare prints it to stdout.",
	})
	out := fs.String("out", "", "write the SVG to this file")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	svg := menuSVG()
	switch {
	case *out != "":
		if err := os.WriteFile(*out, svg, 0o644); err != nil {
			return helptext.Fail("help-svg: %v", err)
		}
	default:
		if _, err := os.Stdout.Write(svg); err != nil {
			return helptext.Fail("help-svg: %v", err)
		}
	}
	return 0
}

func menuSVGSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.Plain("out")}}
}
