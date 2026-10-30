package browser

import (
	"fmt"
	"io"
)

// RenderText prints one line per known browser plus the resolved system default, for `web browsers`.
// JSON output needs no equivalent here: Report's json tags are the shape (internal/helptext.WriteJSON(w, report)).
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
