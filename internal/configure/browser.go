package configure

import "github.com/edotau/claude-code/internal/browser"

// runBrowser reports which installed browsers Claude in Chrome can drive and the system default — read-only,
// never fails --check; the extension install itself needs the user's click (claude-code network browser --setup).
func runBrowser(d *dialog) bool {
	rep := browser.Detect(browser.NewEnv())
	for _, b := range rep.Browsers {
		if !b.Installed {
			continue
		}
		mark := ""
		if b.Default {
			mark = "  (default)"
		}
		d.printf("%-10s extension=%-13s cdp=%-13s gui=%s%s\n", b.Name, b.Paths.Extension, b.Paths.CDP, b.Paths.GUI, mark)
	}
	switch rep.Default {
	case "":
		d.printf("default     not a Chromium-family browser → claude-code network browser for the control paths\n")
	default:
		d.printf("default     %s → claude-code network browser --setup --browser default installs the extension\n", rep.Default)
	}
	return true
}
