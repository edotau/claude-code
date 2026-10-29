package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agenttree"
	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/web"
)

// cmdNetworkBrowser lists every installed browser's control paths and the system default; --setup repairs one.
func cmdNetworkBrowser(args []string) int {
	fs := helptext.NewFlagSet("network browser", helptext.Page{Usage: "claude-code network browser [flags]",
		About: "List every installed browser's control paths and the system default; --setup\n" +
			"repairs what the harness owns for one and opens its extension page; --extract\n" +
			"scrapes records from a page (--format jsonl|csv|json there)."})
	setup := fs.Bool("setup", false, "repair what the harness owns for one browser and open its extension page")
	name := fs.String("browser", "default", "chrome|edge|brave|arc|vivaldi|chromium|default (with --setup)")
	dryRun := fs.Bool("dry-run", false, "describe the actions without running them (with --setup)")
	format := outfmt.Flag(fs)
	ex := addExtractFlags(fs)
	if _, err := outfmt.Parse(fs, args); err != nil {
		return helptext.ExitCode(err)
	}
	if *ex.url != "" {
		if *setup {
			return failUsage("network browser: --extract and --setup are mutually exclusive")
		}
		fmtSet := ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "format" {
				fmtSet = *format
			}
		})
		return networkBrowserExtract(ex, fmtSet)
	}
	if bad := extractFlagsSet(fs); bad != "" {
		return failUsage("network browser: --%s belongs to --extract", bad)
	}
	if err := outfmt.Check(*format); err != nil {
		fail("network browser: %v", err)
		return 2
	}
	if !*setup {
		bad := ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "browser" || f.Name == "dry-run" {
				bad = f.Name
			}
		})
		if bad != "" {
			return failUsage("network browser: --%s belongs to --setup", bad)
		}
		return networkBrowserList(*format)
	}
	return networkBrowserSetup(*name, *dryRun, *format)
}

func networkBrowserList(format string) int {
	rep := browser.Detect(browser.NewEnv())
	if format == "json" {
		if err := outfmt.Write(os.Stdout, rep); err != nil {
			return fail("network browser: %v", err)
		}
		return 0
	}
	browser.RenderText(os.Stdout, rep)
	return 0
}

// networkBrowserSetup repairs what the harness owns for one browser and opens the extension install page.
func networkBrowserSetup(name string, dryRun bool, format string) int {
	env := browser.NewEnv()
	env.DryRun = dryRun
	acts, err := browser.Setup(env, name)
	if err != nil {
		return fail("network browser --setup: %v", err)
	}
	if format == "json" {
		if err := outfmt.Write(os.Stdout, acts); err != nil {
			return fail("network browser --setup: %v", err)
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

var networkViewPage = helptext.Page{Usage: "claude-code network view [flags] <agentId|prefix>",
	About: "Print one subagent's transcript snapshot; --follow then streams its live events."}

// cmdNetworkView is the drill-down into one subagent: a snapshot, then with --follow its live event stream.
func cmdNetworkView(args []string) int {
	fs := helptext.NewFlagSet("network view", networkViewPage)
	session := fs.String("session", "", "session id to search (default: every session in --days)")
	days := fs.Int("days", 1, "session scope in days")
	n := fs.Int("n", 40, "trailing events in the snapshot")
	follow := fs.Bool("follow", false, "after the snapshot, stream new events until the agent finishes")
	format := outfmt.Flag(fs)
	rest, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		fail("network view: %v", err)
		return 2
	}
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}
	opts := agenttree.ViewOptions{ClaudeDir: paths.ConfigDir(), Session: *session, Days: *days, Agent: rest[0], N: *n}
	snap, err := agenttree.View(opts)
	if err != nil {
		return fail("network view: %v", err)
	}
	if *format == "json" {
		if err := outfmt.Write(os.Stdout, snap); err != nil {
			return fail("network view: %v", err)
		}
	} else {
		agenttree.RenderSnapshot(os.Stdout, snap)
	}
	if !*follow {
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agenttree.Follow(ctx, opts, os.Stdout); err != nil && ctx.Err() == nil {
		return fail("network view: %v", err)
	}
	return 0
}

func cmdNetworkPage(args []string) int {
	fs := helptext.NewFlagSet("network page", helptext.Page{Usage: "claude-code network page [flags]",
		About: "Serve the agent page and dashboard on the tailnet."})
	addr := fs.String("addr", "", "bind address (default: this node's tailnet IPv4 on port 7777)")
	hosts := fs.String("hosts", "", "comma-separated dispatch hosts (default: tailnet machines)")
	days := fs.Int("days", 1, "agent-tree scope in days")
	dryRun := fs.Bool("dry-run-dispatch", false, "POST /api/dispatch returns the ssh argv instead of running it")
	if code, done := helptext.Parse(fs, args); done {
		return code
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
		return fail("network page: %v", err)
	}
	return 0
}
