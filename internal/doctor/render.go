package doctor

import (
	"fmt"
	"io"
	"regexp"

	"github.com/edotau/claude-code/internal/ui/ansi"
)

// warnRowRE matches the "warn" word a soft row leads its degraded detail with (agent version, dead user login).
var warnRowRE = regexp.MustCompile(`^warn |· warn `)

// printCheckLines writes checks as ✓/⚠/✗ lines (⚠ = soft row worded "warn"), so all doctor checks print one format.
func printCheckLines(w io.Writer, checks []check) {
	color := ansi.Enabled(w)
	for _, c := range checks {
		mark := ansi.Style(color, ansi.Green, "✓")
		switch {
		case c.OK:
		case !c.Hard && warnRowRE.MatchString(c.Detail):
			mark = ansi.Style(color, ansi.Yellow, "⚠")
		default:
			mark = ansi.Style(color, ansi.Red, "✗")
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, ansi.Status(color, c.Detail))
	}
}
