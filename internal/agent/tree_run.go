package agent

import (
	"fmt"
	"io"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
)

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

// page is `agents tree -h`.
var page = helptext.Page{
	Usage: "claude-code agents tree [flags]",
	About: "Live tree of running Claude sessions, their subagents and dispatched vendor legs, in every repo. " +
		"--background adds idle and finished work.",
}

// RunTree is `claude-code agents tree`; claudeDir is injectable so tests never touch the real
// ~/.claude/projects tree.
func RunTree(args []string, claudeDir string, out, errw io.Writer) int {
	fs := helptext.NewFlagSet("agents tree", page)
	fs.SetOutput(errw)
	days := fs.Int("days", 1, "sessions modified in the last N days (ignored with --session)")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	var background bool
	fs.BoolVar(&background, "background", false, "also show idle and finished work in the --days window")
	fs.BoolVar(&background, "bg", false, "shorthand for --background")
	watch := fs.Bool("watch", false, "clear and re-render on --interval (text only)")
	interval := fs.Duration("interval", 2*time.Second, "redraw interval for --watch")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		fmt.Fprintln(errw, "agents tree:", err)
		return 2
	}
	if *watch && *format == outfmt.JSON {
		fmt.Fprintln(errw, "agents tree: --watch has no JSON form; drop --format json")
		return 2
	}
	opts := Options{Days: *days, Session: *session, Running: !background && *session == ""} // a named session shows whole
	all := background || *session != ""

	if !*watch {
		return renderOnce(claudeDir, opts, *format, all, out, errw)
	}
	for {
		fmt.Fprint(out, clearScreen)
		if code := renderOnce(claudeDir, opts, *format, all, out, errw); code != 0 {
			return code
		}
		time.Sleep(*interval)
	}
}

// renderOnce builds the tree once and writes it in the requested format; showAll skips the
// default idle-with-nothing-dispatched filter.
func renderOnce(claudeDir string, opts Options, format string, showAll bool, out, errw io.Writer) int {
	tree, err := Build(claudeDir, opts)
	if err != nil {
		fmt.Fprintln(errw, "agents tree:", err)
		return 1
	}
	if !showAll {
		tree.Sessions = filterIdleSessions(tree.Sessions)
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	if opts.Session == "" { // every view lists them, so --background stays a superset of the default
		tree.Dispatches = RunningDispatches(now)
		tree.Sessions = dropDispatchSessions(tree.Sessions, tree.Dispatches)
	}
	if format == outfmt.JSON {
		return boolToExit(outfmt.Write(out, tree) == nil)
	}
	RenderText(out, tree, now)
	return 0
}

func boolToExit(ok bool) int {
	if ok {
		return 0
	}
	return 1
}
