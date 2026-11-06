package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/dispatch"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/live"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/ui/helptext"
	uitree "github.com/edotau/claude-code/internal/ui/tree"
)

func agentsCancel(rec agent.Record, format, cwd string) int {
	rec, err := job.CancelJob(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return helptext.Fail("agents cancel: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}{rec.ID, rec.Status, rec.Error}); err != nil {
			return helptext.Fail("agents cancel: %v", err)
		}
		return 0
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// agentsJobPage returns `agents <verb> -h`.
func agentsJobPage(verb string) helptext.Page {
	about := map[string]string{
		"status": "Show one dispatch's state, whichever agent ran it (claude included).",
		"result": "Print a finished dispatch's output; --format json prints its envelope.",
		"tail":   "Print a dispatch's log; --format json streams its events.jsonl instead.",
		"cancel": "Interrupt a running background dispatch, whichever agent runs it.",
	}[verb]
	return helptext.Page{Usage: "claude-code agents " + verb + " <id> [flags]", About: about}
}

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

func agentsResult(rec agent.Record, format, cwd string) int {
	rec, log, err := job.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, agent.RecordEnvelope(rec, os.Stderr)); err != nil {
			return helptext.Fail("agents result: %v", err)
		}
		return ResultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if line := agent.RecordEnvelope(rec, os.Stderr).RanOn; line != "" {
		fmt.Println("ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: claude-code agents ask --agent %s --session %s\n", rec.Agent, rec.SessionID)
	}
	return ResultExit(rec)
}

// agentsSendPage is `agents send -h`.
var agentsSendPage = helptext.Page{
	Usage: "claude-code agents send <id> <task | -> [flags]",
	About: "Send one user turn to a live claude or gemini session (ask --agent <a> --live) and stream " +
		"that turn's events; exits on its result. One turn at a time per session.",
}

func agentsStatus(rec agent.Record, format, cwd string) int {
	rows, err := job.Status(context.Background(), rec.Agent, cwd, rec.ID, true)
	if err != nil {
		return helptext.Fail("agents status: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rows); err != nil {
			return helptext.Fail("agents status: %v", err)
		}
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, StatusDetail(r))
	}
	return 0
}

// agentsSyncPage is `agents sync -h`.
var agentsSyncPage = helptext.Page{
	Usage: "claude-code agents sync [flags]",
	About: "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills.",
}

func agentsTail(rec agent.Record, format string, follow bool, cwd string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if format == helptext.JSON {
		rec, err = job.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = job.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
	}
	if err != nil {
		return helptext.Fail("agents tail: %v", err)
	}
	return TailExit(rec, follow)
}

// agentsTreePage is `agents tree -h`.
var agentsTreePage = helptext.Page{
	Usage: "claude-code agents tree [flags]",
	About: "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--background adds idle and finished work.",
}

// clearScreen re-homes the cursor and wipes the terminal between --watch redraws.
const clearScreen = "\x1b[H\x1b[2J"

func CmdAgents(args []string) int {
	if len(args) == 0 {
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	switch args[0] {
	case "list":
		return cmdAgentsList(args[1:])
	case "ask":
		return ask.Cmd(args[1:])
	case "dispatch":
		return dispatch.Cmd(args[1:])
	case "sync":
		return cmdAgentsSync(args[1:])
	case "tree":
		return Tree(args[1:])
	case "reap":
		return cmdAgentsReap(args[1:])
	case "status", "result", "tail", "cancel":
		return cmdAgentsJob(args[0], args[1:])
	case "send":
		return cmdAgentsSend(args[1:])
	case "-h", "--help", "help":
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	helptext.Fail("agents: unknown subcommand %q", args[0])
	agentsPage.Fprint(os.Stderr)
	return 1
}

// cmdAgentsJob is `claude-code agents status|result|tail|cancel <id>`: the agent comes from the dispatch record.
func cmdAgentsJob(verb string, args []string) int {
	fs := helptext.NewFlagSet("agents "+verb, agentsJobPage(verb))
	format := helptext.FormatFlag(fs)
	follow := new(bool)
	if verb == "tail" {
		follow = fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	}
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents "+verb, err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	rec, err := job.Find(pos[0])
	if err != nil {
		return helptext.Fail("agents %s: %v", verb, err)
	}
	cwd, _ := os.Getwd()
	switch verb {
	case "status":
		return agentsStatus(rec, *format, cwd)
	case "result":
		return agentsResult(rec, *format, cwd)
	case "tail":
		return agentsTail(rec, *format, *follow, cwd)
	}
	return agentsCancel(rec, *format, cwd)
}

// cmdAgentsList is `agents list`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := helptext.NewFlagSet("agents list", agentsListPage)
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents list --format)")
	remoteBin := fs.String("remote-bin", "", "remote claude-code path for --host (default $"+agent.RemoteBinEnv+", then claude-code)")
	format := fs.String("format", "text", "text, or json: per agent {name, binary, available, reason, caps, events}")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || !slices.Contains([]string{helptext.Text, helptext.JSON}, *format) {
		fs.Usage()
		return 2
	}
	if *host != "" {
		if err := agent.ListRemote(*host, agent.RemoteBin(*remoteBin), *format, os.Stdout); err != nil {
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

// cmdAgentsSend is `claude-code agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := helptext.NewFlagSet("agents send", agentsSendPage)
	format := fs.String("format", helptext.Text, "stream text, print the JSON envelope, or stream-json NDJSON events")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains([]string{helptext.Text, helptext.JSON, agent.FormatStreamJSON}, *format) {
		fs.Usage()
		return 2
	}
	prompt, err := agent.ResolveAskPrompt(strings.Join(pos[1:], " "))
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	id := pos[0]
	if rec, ferr := job.Find(id); ferr == nil {
		id = rec.ID
	}
	env, err := live.SendLive(ctx, id, prompt, ask.StdoutSink(*format, id))
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	printLiveEnvelope(env, *format)
	if env.Status != agent.StatusOK {
		return 1
	}
	return 0
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
	rows := agent.RosterRows()
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents", rows)
	}
	for _, r := range rows {
		fmt.Println(r.Line())
	}
	return 0
}

func printLiveEnvelope(env agent.Envelope, format string) {
	if format == helptext.JSON {
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
	} else if format == helptext.Text && env.Answer != "" && !strings.HasSuffix(env.Answer, "\n") {
		fmt.Println()
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

// ResultExit reports a failed job's error on stderr and returns 1; a finished ok job returns 0.
func ResultExit(rec agent.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", shapes.Redact(rec.Error)) // the stderr tail in it may carry a key
	return 1
}

// StatusDetail is a status row's tail: progress and preview, or the preview plus the one-line error text.
func StatusDetail(r job.Row) string {
	d := r.Preview
	if r.Progress != "" {
		d = "[" + r.Progress + "] " + d
	}
	if r.Error != "" {
		d += " -- " + shapes.Preview(r.Error, 300)
	}
	return d
}

// TailExit is a tail's exit code: 1 for an errored job, or for a followed job that is still "running" (lost).
func TailExit(rec agent.Record, follow bool) int {
	if rec.Status == "running" && follow {
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	return ResultExit(rec)
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
