package tree

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

// page is `agents tree -h`.
var page = helptext.Page{
	Usage: "claude-code agents tree [flags]",
	About: "Live tree of running Claude sessions, their subagents and dispatched vendor legs, in every repo. " +
		"--background adds idle and finished work.",
}

// Run is `claude-code agents tree`; claudeDir is injectable so tests never touch the real
// ~/.claude/projects tree.
func Run(args []string, claudeDir string, out, errw io.Writer) int {
	fs := helptext.NewFlagSet("agents tree", page)
	fs.SetOutput(errw)
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
		fmt.Fprintln(errw, "agents tree:", err)
		return 2
	}
	if *watch && *format == helptext.JSON {
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
	if format == helptext.JSON {
		return boolToExit(helptext.WriteJSON(out, tree) == nil)
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

// regEntry is one ~/.claude/sessions/<pid>.json: Claude Code's own record of a running CLI process.
type regEntry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Status    string `json:"status"` // idle|busy|waiting
	Kind      string `json:"kind"`   // interactive|bg; Claude Code's own key, surfaced as Session.Mode
}

// pidAlive is a var so tests can stub the liveness probe.
var pidAlive = proc.Alive

// readRegistry returns the live sessions/*.json entries keyed by session id; unreadable or dead ones are skipped.
func readRegistry(claudeDir string) map[string]regEntry {
	out := map[string]regEntry{}
	files, _ := filepath.Glob(filepath.Join(claudeDir, "sessions", "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e regEntry
		if json.Unmarshal(data, &e) != nil || e.PID <= 0 || e.SessionID == "" || !pidAlive(e.PID) {
			continue
		}
		out[e.SessionID] = e
	}
	return out
}

// applyRegistry copies a live registry entry onto s; a busy CLI is active, any other live state idle.
func applyRegistry(s Session, e regEntry) Session {
	s.Name, s.PID, s.Live, s.CLIStatus, s.Mode = e.Name, e.PID, true, e.Status, e.Kind
	if s.Cwd == "" {
		s.Cwd = e.Cwd
	}
	s.Status = statusIdle
	if e.Status == "busy" {
		s.Status = statusActive
	}
	return s
}

// liveTranscripts finds projects/*/<id>.jsonl for each registry entry not already in have.
func liveTranscripts(claudeDir string, reg map[string]regEntry, have map[string]bool) []string {
	var files []string
	for id := range reg {
		if have[id] || id != filepath.Base(id) || id == ".." {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
		files = append(files, matches...)
	}
	return files
}
