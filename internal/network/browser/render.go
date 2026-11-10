package browser

import (
	"fmt"
	"io"
)

// renderText prints one line per known browser plus the system default; it returns the first write error.
func renderText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "default: %s\n", orDash(r.Default)); err != nil {
		return err
	}
	for _, b := range r.Browsers {
		if err := renderBrowserLine(w, b); err != nil {
			return err
		}
	}
	return nil
}

func renderBrowserLine(w io.Writer, b Browser) error {
	mark := ""
	if b.Default {
		mark = "  (default)"
	}
	if _, err := fmt.Fprintf(w, "%-10s installed=%-5v host=%-13s extension=%-13s cdp=%-13s webdriver=%-13s gui=%-13s%s\n",
		b.Name, b.Installed, b.Host, b.Paths.Extension, b.Paths.CDP, b.Paths.WebDriver, b.Paths.GUI, mark); err != nil {
		return err
	}
	for _, p := range b.Profiles {
		if _, err := fmt.Fprintf(w, "  profile %-12s extension=%s\n", p.Name, p.Extension); err != nil {
			return err
		}
	}
	return nil
}
