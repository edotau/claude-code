package browser

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// browserFlags declares `network browser`'s flags on fs.
func browserFlags(fs *flag.FlagSet) (open *string, setup *bool, name *string, dryRun *bool, format *string) {
	open = fs.String("open", "", "open an http(s) URL or file: VS Code's client, this desktop, else print")
	setup = fs.Bool("setup", false, "repair what the harness owns for one browser and open its extension page")
	name = fs.String("browser", "default", "chrome|edge|brave|arc|vivaldi|chromium|default (with --setup)")
	dryRun = fs.Bool("dry-run", false, "describe the actions without running them (with --setup)")
	format = helptext.FormatFlag(fs)
	return
}

// Flags are the completion flags of `network browser`, derived from its FlagSet.
func Flags() []helptext.Flag {
	fs := helptext.NewFlagSet("network browser", helptext.Page{})
	browserFlags(fs)
	return helptext.FlagsOf(fs, map[string]func() []string{"browser": func() []string {
		return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
	}})
}

// Cmd lists every installed browser's control paths and the system default; --setup repairs one.
func Cmd(args []string) int {
	fs := helptext.NewFlagSet("network browser", helptext.Page{Usage: "claude-code network browser [flags]",
		About: "List every installed browser's control paths and the system default; --setup\n" +
			"repairs what the harness owns for one and opens its extension page; --open\n" +
			"opens a URL where you sit ($BROWSER=~/.claude/bin/browser runs it)."})
	open, setup, name, dryRun, format := browserFlags(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 0 {
		return helptext.FailUsage("network browser: unexpected argument %q (use --browser)", rest[0])
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("network browser", err)
	}
	if *open != "" {
		if setupFlagsSet(fs) != "" || *setup {
			return helptext.FailUsage("network browser: --open takes no --setup flags")
		}
		registerDeviceHosts()
		return openTarget(*open)
	}
	return networkBrowserListOrSetup(fs, *setup, *name, *dryRun, *format)
}

// openTarget opens an http(s) URL or an existing file and nothing else; target reaches the opener as one argument.
func openTarget(target string) int {
	var err error
	if u, perr := url.Parse(target); perr == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		err = proc.OpenURL(target, proc.OpenView)
	} else if abs, aerr := filepath.Abs(target); aerr == nil && regularFile(abs) {
		err = proc.OpenFile(abs)
	} else {
		return helptext.FailUsage("network browser --open: %q is neither an http(s) URL nor a file", target)
	}
	if err != nil {
		return helptext.Fail("network browser --open: %v", err)
	}
	return 0
}

func regularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
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
	rep := Detect(NewEnv())
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return helptext.Fail("network browser: %v", err)
		}
		return 0
	}
	if err := renderText(os.Stdout, rep); err != nil {
		return helptext.Fail("network browser: %v", err)
	}
	return 0
}

// networkBrowserSetup repairs what the harness owns for one browser and opens the extension install page.
func networkBrowserSetup(name string, dryRun bool, format string) int {
	env := NewEnv()
	env.DryRun = dryRun
	acts, err := Setup(env, name)
	if err != nil {
		return helptext.Fail("network browser --setup: %v", err)
	}
	rc := setupExitCode(acts.Items)
	if format == "json" {
		if err := helptext.WriteJSON(os.Stdout, acts); err != nil {
			return helptext.Fail("network browser --setup: %v", err)
		}
		return rc
	}
	fmt.Printf("%s:\n", acts.Browser)
	for _, a := range acts.Items {
		fmt.Println(setupLine(a))
	}
	return rc
}

// setupExitCode is 1 when any action failed or needs the user's hand.
func setupExitCode(items []action) int {
	for _, a := range items {
		if a.Error != "" || a.Kind == "missing-host" || a.Kind == "enable-extension" {
			return 1
		}
	}
	return 0
}

func setupLine(a action) string {
	line := "  " + a.Kind + ": " + a.Description
	if len(a.Command) > 0 {
		line += "  [" + strings.Join(a.Command, " ") + "]"
	}
	if a.Error != "" {
		line += "  error: " + a.Error
	}
	return line
}

// renderText prints one line per known browser plus the system default; it returns the first write error.
func renderText(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "default: %s\n", orDash(r.Default)); err != nil {
		return err
	}
	if r.DefaultErr != "" {
		if _, err := fmt.Fprintf(w, "default detection failed: %s\n", r.DefaultErr); err != nil {
			return err
		}
	}
	for _, b := range r.Browsers {
		if err := renderBrowserLine(w, b); err != nil {
			return err
		}
	}
	return nil
}

func renderBrowserLine(w io.Writer, b browser) error {
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
