package network

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdNetworkBrowser lists every installed browser's control paths and the system default; --setup repairs one.
func cmdNetworkBrowser(args []string) int {
	fs := helptext.NewFlagSet("network browser", helptext.Page{Usage: "claude-code network browser [flags]",
		About: "List every installed browser's control paths and the system default; --setup\n" +
			"repairs what the harness owns for one and opens its extension page."})
	setup := fs.Bool("setup", false, "repair what the harness owns for one browser and open its extension page")
	name := fs.String("browser", "default", "chrome|edge|brave|arc|vivaldi|chromium|default (with --setup)")
	dryRun := fs.Bool("dry-run", false, "describe the actions without running them (with --setup)")
	format := helptext.FormatFlag(fs)
	if _, err := helptext.ParseArgs(fs, args); err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("network browser", err)
	}
	return networkBrowserListOrSetup(fs, *setup, *name, *dryRun, *format)
}

func networkBrowserListOrSetup(fs *flag.FlagSet, setup bool, name string, dryRun bool, format string) int {
	if setup {
		return networkBrowserSetup(name, dryRun, format)
	}
	if bad := setupFlagsSet(fs); bad != "" {
		return helptext.FailUsage("network browser: --%s belongs to --setup", bad)
	}
	return networkBrowserList(format)
}

// setupFlagsSet names the last setup-only flag given, "" when none.
func setupFlagsSet(fs *flag.FlagSet) string {
	bad := ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "browser" || f.Name == "dry-run" {
			bad = f.Name
		}
	})
	return bad
}

func networkBrowserList(format string) int {
	rep := browser.Detect(browser.NewEnv())
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return helptext.Fail("network browser: %v", err)
		}
		return 0
	}
	if err := browser.RenderText(os.Stdout, rep); err != nil {
		return helptext.Fail("network browser: %v", err)
	}
	return 0
}

// networkBrowserSetup repairs what the harness owns for one browser and opens the extension install page.
func networkBrowserSetup(name string, dryRun bool, format string) int {
	env := browser.NewEnv()
	env.DryRun = dryRun
	acts, err := browser.Setup(env, name)
	if err != nil {
		return helptext.Fail("network browser --setup: %v", err)
	}
	rc := 0
	for _, a := range acts.Items {
		if a.Error != "" {
			rc = 1
		}
	}
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, acts); err != nil {
			return helptext.Fail("network browser --setup: %v", err)
		}
		return rc
	}
	fmt.Printf("%s:\n", acts.Browser)
	for _, a := range acts.Items {
		line := "  " + a.Kind + ": " + a.Description
		if len(a.Command) > 0 {
			line += "  [" + strings.Join(a.Command, " ") + "]"
		}
		if a.Error != "" {
			line += "  error: " + a.Error
		}
		fmt.Println(line)
	}
	return rc
}
