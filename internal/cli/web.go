package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/web"
)

const webUsage = `usage: claude-code web [--addr ip:port] [--hosts h1,h2] [--days N]
       claude-code web browsers [--format text|json]
       claude-code web setup [--browser chrome|edge|brave|arc|vivaldi|chromium|default] [--dry-run] [--format text|json]`

// cmdWeb is the one agent-web verb: serve the dashboard, or the browser-control subcommands it also hosts.
func cmdWeb(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "browsers":
			return cmdWebBrowsers(args[1:])
		case "setup":
			return cmdWebSetup(args[1:])
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, webUsage)
			return 2
		}
	}
	return cmdWebServe(args)
}

// cmdWebBrowsers reports every installed browser's control paths and the system default.
func cmdWebBrowsers(args []string) int {
	fs := flag.NewFlagSet("web browsers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := outfmt.Flag(fs)
	if _, err := outfmt.Parse(fs, args); err != nil {
		return 2
	}
	rep := browser.Detect(browser.NewEnv())
	if *format == "json" {
		if err := outfmt.Write(os.Stdout, rep); err != nil {
			return fail("web browsers: %v", err)
		}
		return 0
	}
	browser.RenderText(os.Stdout, rep)
	return 0
}

// cmdWebSetup repairs what the harness owns for one browser and opens the extension install page.
func cmdWebSetup(args []string) int {
	fs := flag.NewFlagSet("web setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("browser", "default", "chrome|edge|brave|arc|vivaldi|chromium|default (the system default)")
	dryRun := fs.Bool("dry-run", false, "describe the actions without running them")
	format := outfmt.Flag(fs)
	if _, err := outfmt.Parse(fs, args); err != nil {
		return 2
	}
	env := browser.NewEnv()
	env.DryRun = *dryRun
	acts, err := browser.Setup(env, *name)
	if err != nil {
		return fail("web setup: %v", err)
	}
	if *format == "json" {
		if err := outfmt.Write(os.Stdout, acts); err != nil {
			return fail("web setup: %v", err)
		}
		return 0
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
	return 0
}

func cmdWebServe(args []string) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", "", "bind address (default: this node's tailnet IPv4 on port 7777)")
	hosts := fs.String("hosts", "", "comma-separated dispatch hosts (default: tailnet machines)")
	days := fs.Int("days", 1, "agent-tree scope in days")
	dryRun := fs.Bool("dry-run-dispatch", false, "POST /api/dispatch returns the ssh argv instead of running it")
	if fs.Parse(args) != nil {
		return 2
	}
	var hostList []string
	if *hosts != "" {
		hostList = strings.Split(*hosts, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := web.Run(ctx, web.Options{
		Addr: *addr, Hosts: hostList, Days: *days, DryRunDispatch: *dryRun,
		Out: os.Stdout, ErrOut: os.Stderr,
	}); err != nil {
		return fail("web: %v", err)
	}
	return 0
}
