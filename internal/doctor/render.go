package doctor

import (
	"fmt"
	"io"
	"regexp"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// warnRowRE matches the "warn" word a soft row leads its degraded detail with (agent version, dead user login).
var warnRowRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^warn |· warn `) })

// termWidth is a var so tests can fake a terminal width.
var termWidth = helptext.TermWidth

// printCheckLines writes checks as ✓/⚠/✗ lines (⚠ = soft row worded "warn"), so all doctor checks print one format.
func printCheckLines(w io.Writer, checks []check) {
	color := ansi.Enabled(w)
	term := termWidth(w)
	for _, c := range checks {
		mark := ansi.Style(color, ansi.Green, "✓")
		switch {
		case c.OK:
		case !c.Hard && warnRowRE().MatchString(c.Detail):
			mark = ansi.Style(color, ansi.Yellow, "⚠")
		default:
			mark = ansi.Style(color, ansi.Red, "✗")
		}
		detail := c.Detail
		if term > 0 && c.OK { // a warning or failure keeps its whole text: the fix is usually at the end
			detail = textutil.TruncateCells(detail, min(term, 80)-textutil.DisplayWidth("✓ "+c.Name+": "), "...")
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, ansi.Status(color, detail))
	}
}
