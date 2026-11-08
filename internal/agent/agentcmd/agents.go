package agentcmd

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/dispatch"
	"github.com/edotau/claude-code/internal/agent/lifecycle"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/ui/helptext"
	uitree "github.com/edotau/claude-code/internal/ui/tree"
)

// agentsListPage is `agents list -h`.
var agentsListPage = helptext.Page{
	Usage: "claude-code agents list [flags]",
	About: "List each agent, whether it is available and which request fields (model, profile, effort, …) it honors.",
}

// agentsPage is `agents -h`, also printed by bare `agents`.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "Run, track and list the agents (claude, codex, copilot, gemini, opencode), " +
		"and sync the roster into their homes.",
	Commands: [][2]string{
		{"list", "list each agent, its availability and honored fields; --format json"},
		{"ask", "one-shot task on an agent; --format json = envelope"},
		{"dispatch", "run a task by role: explore, fleet, compare"},
		{"sync", "project agents/ + CLAUDE.md into every agent home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"reap", "mark dead running dispatches as error; drop stale markers"},
		{"status", "show a dispatch's state (any agent): status <id>"},
		{"result", "print a finished dispatch's answer or envelope: result <id>"},
		{"tail", "follow a dispatch's log or events: tail <id> [-f]"},
		{"cancel", "interrupt a running dispatch: cancel <id>"},
		{"send", "send one turn to a live claude or gemini session: send <id> <task>"},
	},
	Hint: "claude-code agents <command> -h lists that command's flags.",
}

// agentsReapPage is `agents reap -h`.
var agentsReapPage = helptext.Page{
	Usage: "claude-code agents reap [flags]",
	About: "Mark dispatch records still \"running\" whose process is gone as error, and drop stale " +
		"agent-inflight markers.",
}

// agentsSyncPage is `agents sync -h`.
var agentsSyncPage = helptext.Page{
	Usage: "claude-code agents sync [flags]",
	About: "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills.",
}

// agentsTreePage is `agents tree -h`.
var agentsTreePage = helptext.Page{
	Usage: "claude-code agents tree [flags]",
	About: "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--background adds idle and finished work.",
}

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

// Run is `claude-code agents`.
func Run(args []string) int {
	if len(args) == 0 {
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	if run := agentsVerbs[args[0]]; run != nil {
		return run(args[1:])
	}
	if slices.Contains([]string{"-h", "--help", "help"}, args[0]) {
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	helptext.Fail("agents: unknown subcommand %q", args[0])
	agentsPage.Fprint(os.Stderr)
	return 1
}

// agentsVerbs routes `agents <verb>` to its handler; the job verbs share lifecycle.Job.
var agentsVerbs = map[string]func([]string) int{
	"list": cmdAgentsList, "ask": ask.Cmd, "dispatch": dispatch.Cmd, "sync": cmdAgentsSync,
	"tree": cmdAgentsTree, "reap": cmdAgentsReap, "send": lifecycle.Send,
	"status": func(a []string) int { return lifecycle.Job("status", a) },
	"result": func(a []string) int { return lifecycle.Job("result", a) },
	"tail":   func(a []string) int { return lifecycle.Job("tail", a) },
	"cancel": func(a []string) int { return lifecycle.Job("cancel", a) },
}

// cmdAgentsList is `agents list`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := helptext.NewFlagSet("agents list", agentsListPage)
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents list --format)")
	remoteBin := fs.String("remote-bin", "", "remote claude-code path for --host (default $"+client.RemoteBinEnv+", then claude-code)")
	format := fs.String("format", "text", "text, or json: per agent {name, binary, available, reason, caps, events}")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || !slices.Contains([]string{helptext.Text, helptext.JSON}, *format) {
		fs.Usage()
		return 2
	}
	if *host != "" {
		if err := client.ListRemote(*host, client.RemoteBin(*remoteBin), *format, os.Stdout); err != nil {
			return helptext.Fail("%v", err)
		}
		return 0
	}
	return listAgents(*format)
}

// cmdAgentsReap marks dead-but-"running" dispatch records as error and drops stale agent-inflight markers.
func cmdAgentsReap(args []string) int {
	fs := helptext.NewFlagSet("agents reap", agentsReapPage)
	dryRun := fs.Bool("dry-run", false, "print what would change, write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return tree.Reap(*dryRun, os.Stdout, os.Stderr)
}

// cmdAgentsSync renders agents/ into state/projections, installs the copies into agent homes and links skills/.
func cmdAgentsSync(args []string) int {
	fs := helptext.NewFlagSet("agents sync", agentsSyncPage)
	name := fs.String("agent", "", "one target: "+strings.Join(project.Names(), "|")+" (default: all)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	noPrune := fs.Bool("no-prune", false, "keep orphaned projections")
	format := fs.String("format", "text", "output: text|json (json = one row per destination)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || (*format != "text" && *format != "json") {
		fs.Usage()
		return 2
	}
	return project.Sync(project.Options{JSON: *format == "json", DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Agent: *name, Out: os.Stdout})
}

// listAgents prints the roster as text, or as the JSON array of rows.
func listAgents(format string) int {
	rows := client.RosterRows()
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents", rows)
	}
	for _, r := range rows {
		fmt.Println(r.Line())
	}
	return 0
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

// checkTreeFlags rejects an invalid --format and the --watch combinations that cannot run.
func checkTreeFlags(format string, watch bool, interval time.Duration) error {
	if err := helptext.CheckFormat(format); err != nil {
		return err
	}
	if watch && format == helptext.JSON {
		return errors.New("--watch has no JSON form; drop --format json")
	}
	if watch && interval <= 0 {
		return errors.New("--interval must be positive")
	}
	return nil
}

// cmdAgentsTree is `agents tree`.
func cmdAgentsTree(args []string) int {
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
	if err := checkTreeFlags(*format, *watch, *interval); err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 2
	}
	// A named session shows whole.
	opts := tree.Options{Days: *days, Session: *session, Running: !background && *session == ""}
	all := background || *session != ""

	if !*watch {
		return renderAgentsTree(opts, *format, all)
	}
	return watchAgentsTree(opts, *format, all, *interval)
}

// watchAgentsTree redraws the tree every interval until a render fails.
func watchAgentsTree(opts tree.Options, format string, showAll bool, interval time.Duration) int {
	for {
		fmt.Print(clearScreen)
		if code := renderAgentsTree(opts, format, showAll); code != 0 {
			return code
		}
		time.Sleep(interval)
	}
}
