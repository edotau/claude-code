// Package agents holds the `claude-code agents` subcommand handlers.
package agents

import (
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
	uitree "github.com/edotau/claude-code/internal/ui/tree"
)

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

// agentsTreePage is `agents tree -h`.
var agentsTreePage = helptext.Page{
	Usage: "claude-code agents tree [flags]",
	About: "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--background adds idle and finished work.",
}

// Tree is `claude-code agents tree`.
func Tree(args []string) int {
	fs := helptext.NewFlagSet("agents tree", agentsTreePage)
	days := fs.Int("days", 1, "sessions modified in the last N days (ignored with --session)")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	var background bool
	fs.BoolVar(&background, "background", false, "also show idle and finished work in the --days window")
	fs.BoolVar(&background, "bg", false, "shorthand for --background")
	watch := fs.Bool("watch", false, "clear and re-render on --interval (text only)")
	interval := fs.Duration("interval", 2*time.Second, "redraw interval for --watch")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 2
	}
	if *watch && *format == helptext.JSON {
		fmt.Fprintln(os.Stderr, "agents tree: --watch has no JSON form; drop --format json")
		return 2
	}
	if *watch && *interval <= 0 {
		fmt.Fprintln(os.Stderr, "agents tree: --interval must be positive")
		return 2
	}
	// A named session shows whole.
	opts := tree.Options{Days: *days, Session: *session, Running: !background && *session == ""}
	all := background || *session != ""

	if !*watch {
		return renderAgentsTree(opts, *format, all)
	}
	for {
		fmt.Print(clearScreen)
		if code := renderAgentsTree(opts, *format, all); code != 0 {
			return code
		}
		time.Sleep(*interval)
	}
}

// renderAgentsTree lists the tree once and writes it in the requested format.
func renderAgentsTree(opts tree.Options, format string, showAll bool) int {
	listing, err := tree.Listing(paths.ConfigDir(), opts, showAll)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 1
	}
	if format == helptext.JSON {
		if helptext.WriteJSON(os.Stdout, listing) != nil {
			return 1
		}
		return 0
	}
	uitree.RenderText(os.Stdout, listing, time.Now())
	return 0
}
