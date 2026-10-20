package cli

import (
	"bytes"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/statusline"
)

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
	stops, dim := statusline.EmbeddedTheme(menuSVGPalette)
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
	b.WriteString(`<circle cx="20" cy="18" r="6" fill="#ff5f57"/><circle cx="40" cy="18" r="6" fill="#febc2e"/><circle cx="60" cy="18" r="6" fill="#28c840"/>` + "\n")
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

// cmdMenuSVG prints the menu SVG, writes it (--out), or fails when a file has drifted from it (--check).
func cmdMenuSVG(args []string) int {
	fs := flag.NewFlagSet("help-svg", flag.ContinueOnError)
	out := fs.String("out", "", "write the SVG to this file")
	check := fs.String("check", "", "exit 1 if this file differs from the render")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	svg := menuSVG()
	switch {
	case *check != "":
		got, err := os.ReadFile(*check)
		if err != nil {
			return fail("help-svg: %v", err)
		}
		if !bytes.Equal(got, svg) {
			return fail("help-svg: %s is stale; run `make help-svg`", *check)
		}
	case *out != "":
		if err := os.WriteFile(*out, svg, 0o644); err != nil {
			return fail("help-svg: %v", err)
		}
	default:
		if _, err := os.Stdout.Write(svg); err != nil {
			return fail("help-svg: %v", err)
		}
	}
	return 0
}
