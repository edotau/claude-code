package browser

import (
	"fmt"
	"io"
)

// Need is what the caller wants to do with a browser; SelectPath recommends a control path for it.
// See the package doc comment for the one-line rule per Need.
type Need int

const (
	NeedSignedInSession Need = iota // the user's real, signed-in profile: tabs, cookies, extensions
	NeedAutomation                  // scripted control of the caller's own site, no signed-in session needed
)

// SelectPath recommends a control path for b given need, falling back to gui when nothing better applies.
func SelectPath(b Browser, need Need) string {
	if need == NeedAutomation {
		if b.Family == familyChromium && b.Paths.CDP == stateAvailable {
			return pathCDP
		}
		if b.Paths.WebDriver == stateAvailable {
			return pathWebDriver
		}
		return pathGUI
	}
	if b.Paths.Extension == stateReady {
		return pathExtension
	}
	return pathGUI
}

// RenderText prints one line per known browser plus the resolved system default, for `web browsers`.
// JSON output needs no equivalent here: Report's json tags are the shape (internal/outfmt.Write(w, report)).
func RenderText(w io.Writer, r Report) {
	fmt.Fprintf(w, "default: %s\n", orDash(r.Default))
	for _, b := range r.Browsers {
		renderBrowserLine(w, b)
	}
}

func renderBrowserLine(w io.Writer, b Browser) {
	mark := ""
	if b.Default {
		mark = "  (default)"
	}
	fmt.Fprintf(w, "%-10s installed=%-5v host=%-13s extension=%-13s cdp=%-13s webdriver=%-13s gui=%-13s%s\n",
		b.Name, b.Installed, b.Host, b.Paths.Extension, b.Paths.CDP, b.Paths.WebDriver, b.Paths.GUI, mark)
	for _, p := range b.Profiles {
		fmt.Fprintf(w, "  profile %-12s extension=%s\n", p.Name, p.Extension)
	}
}
