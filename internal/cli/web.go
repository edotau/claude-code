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

const webUsage = `usage: claude-code web [--addr ip:port] [--hosts h1,h2] [--days N]
       claude-code web browsers [--format text|json]
       claude-code web setup [--browser name] [--dry-run] [--format text|json]
       claude-code web view <agentId|prefix> [--session id] [--days N]
                            [--n 40] [--follow] [--format text|json]
       claude-code web search [--query q] [--max-results N]
                              bare = stdio MCP server; --query = one-shot`

// cmdWeb is the one agent-web verb: serve the dashboard, or the browser-control subcommands it also hosts.
func cmdWeb(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "browsers":
			return cmdWebBrowsers(args[1:])
		case "setup":
			return cmdWebSetup(args[1:])
		case "view":
			return cmdWebView(args[1:])
		case "search":
			return cmdWebSearch(args[1:])
		case "-h", "--help":
			helptext.Fprint(os.Stderr, webUsage+"\n")
			return 2
		}
	}
	return cmdWebServe(args)
}

// cmdWebBrowsers reports every installed browser's control paths and the system default.
func cmdWebBrowsers(args []string) int {
	fs := flag.NewFlagSet("web browsers", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of web browsers:\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	if _, err := outfmt.Parse(fs, args); err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("web browsers: %v", err)
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
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of web setup:\n")
		helptext.Defaults(fs)
	}
	name := fs.String("browser", "default", "chrome|edge|brave|arc|vivaldi|chromium|default")
	dryRun := fs.Bool("dry-run", false, "describe the actions without running them")
	format := outfmt.Flag(fs)
	if _, err := outfmt.Parse(fs, args); err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("web setup: %v", err)
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

// cmdWebView is the drill-down into one subagent: a snapshot, then with --follow its live event stream.
func cmdWebView(args []string) int {
	fs := flag.NewFlagSet("web view", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of web view:\n")
		helptext.Defaults(fs)
	}
	session := fs.String("session", "", "session id to search (default: every session in --days)")
	days := fs.Int("days", 1, "session scope in days")
	n := fs.Int("n", 40, "trailing events in the snapshot")
	follow := fs.Bool("follow", false, "after the snapshot, stream new events until the agent finishes")
	format := outfmt.Flag(fs)
	rest, err := outfmt.Parse(fs, args)
	if err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fail("web view: %v", err)
		return 2
	}
	if len(rest) != 1 {
		helptext.Fprint(os.Stderr, "usage: claude-code web view <agentId|prefix> [--session id] [--days N]\n"+
			"                            [--n 40] [--follow] [--format text|json]\n")
		return 2
	}
	opts := agenttree.ViewOptions{ClaudeDir: paths.ConfigDir(), Session: *session, Days: *days, Agent: rest[0], N: *n}
	snap, err := agenttree.View(opts)
	if err != nil {
		return fail("web view: %v", err)
	}
	if *format == "json" {
		if err := outfmt.Write(os.Stdout, snap); err != nil {
			return fail("web view: %v", err)
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
		return fail("web view: %v", err)
	}
	return 0
}

func cmdWebServe(args []string) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of web:\n")
		helptext.Defaults(fs)
	}
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
