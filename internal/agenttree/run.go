package agenttree

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
	About: "Live tree of Claude sessions, their subagents and dispatched vendor legs. " +
		"Idle sessions with nothing dispatched are hidden unless --all.",
}

// Run is `claude-code agents tree`; claudeDir is injectable so tests never touch the real
// ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	fs := helptext.NewFlagSet("agents tree", page)
	fs.SetOutput(errw)
	days := fs.Int("days", 1, "sessions modified in the last N days (ignored with --session)")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	running := fs.Bool("running", false, "keep running sessions, subagents and dispatches (all repos)")
	all := fs.Bool("all", false, "show every session in scope, idle ones included")
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
	opts := Options{Days: *days, Session: *session, Running: *running}

	if !*watch {
		return renderOnce(claudeDir, opts, *format, *all, out, errw)
	}
	for {
		fmt.Fprint(out, clearScreen)
		if code := renderOnce(claudeDir, opts, *format, *all, out, errw); code != 0 {
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
	if opts.Running {
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
