package cli

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// dashboardPage is `agents dashboard -h`.
var dashboardPage = helptext.Page{
	Usage: "claude-code agents dashboard [flags]",
	About: "Walk the live tree of sessions, subagents and dispatches: ↑/↓ (or k/j) move, →/← (or l/h) expand and " +
		"collapse, Enter follows the selected dispatch's output, q or Esc leaves. The listing reloads every " +
		"--interval and the layout follows a terminal resize. Needs a terminal; on Windows it prints the tree once.",
	Hint: "Exit 0; 1 not a terminal or a failed load; 2 usage.",
}

// cmdAgentsDashboard is `agents dashboard`, the interactive tree; `agents tree` stays the plain listing.
func cmdAgentsDashboard(args []string) int {
	fs := helptext.NewFlagSet("agents dashboard", dashboardPage)
	days := fs.Int("days", 1, "last N days; ignored with --session")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	detail := fs.Bool("detail", false, "also show idle, stale and finished work")
	interval := fs.Duration("interval", 2*time.Second, "listing reload interval, at least 500ms")
	files := fs.Bool("files", false, "show each leaf's dirty files and note, and the free files")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || *interval < 500*time.Millisecond {
		fmt.Fprintln(os.Stderr, "agents dashboard: takes no arguments and an --interval of at least 500ms")
		return 2
	}
	opts := tree.Options{Days: *days, Session: *session, Files: *files}
	v := tree.View{All: *detail || *session != "", Interval: *interval}
	if runtime.GOOS == "windows" {
		return tree.Show(paths.ConfigDir(), opts, v)
	}
	if !proc.IsTerminal(os.Stdin) || !proc.IsTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, "agents dashboard: needs a terminal; piped, use `claude-code agents tree`")
		return 1
	}
	return tree.Dashboard(paths.ConfigDir(), opts, v)
}
