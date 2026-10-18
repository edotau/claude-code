package agenttree

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/edotau/claude-code/internal/outfmt"
)

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

// Run is `claude-code agents tree`; claudeDir is injectable so tests never touch the real
// ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("agents tree", flag.ContinueOnError)
	fs.SetOutput(errw)
	days := fs.Int("days", 1, "scope to sessions modified within the last N days (ignored with --session)")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	running := fs.Bool("running", false, "keep only sessions with an active session or a running subagent")
	all := fs.Bool("all", false, "show every session in scope, including idle sessions with no subagents/workflows")
	watch := fs.Bool("watch", false, "clear and re-render on --interval (text only)")
	interval := fs.Duration("interval", 2*time.Second, "redraw interval for --watch")
	format := outfmt.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
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
	if format == outfmt.JSON {
		return boolToExit(outfmt.Write(out, tree) == nil)
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
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
