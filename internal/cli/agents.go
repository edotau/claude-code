package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/configure"
	hookagents "github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// agentsSpec is the completion node for the agents verb.
func agentsSpec() *helptext.Node {
	flag, boolFlag, valuedFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"sync": {Flags: []helptext.Flag{valuedFlag("agent", project.Names), boolFlag("dry-run"), boolFlag("check"),
			formatFlag()}},
		"tree": {Flags: []helptext.Flag{flag("days"), flag("session"), boolFlag("finished"),
			boolFlag("watch"), flag("interval"), formatFlag()}},
		"label": {Flags: []helptext.Flag{flag("model"), flag("task"), flag("session-model"), formatFlag()}},
		"find": {Flags: []helptext.Flag{boolFlag("name"), boolFlag("func"), boolFlag("def"), boolFlag("callers"),
			boolFlag("i"), boolFlag("w"), boolFlag("l"), flag("path"), flag("max"), formatFlag()}},
		"diff": {Flags: []helptext.Flag{flag("base"), flag("path"), flag("verify"), boolFlag("check"), formatFlag()}},
		"list": {Flags: []helptext.Flag{flag("host"), formatFlag()}},
		"ask":  {Flags: ask.CompletionFlags()},
		"dispatch": {Flags: []helptext.Flag{flag("timeout"), formatFlag(), boolFlag("background"),
			flag("waves"), flag("wave"), boolFlag("dry-run"), boolFlag("status"), boolFlag("fresh"), flag("gate"),
			flag("provider"), flag("profile"), flag("profiles"), flag("job"), flag("overlay"), flag("allow"), flag("ask"), flag("deny"), flag("mode"), flag("fallback")}},
		"send":   {Flags: []helptext.Flag{flag("format")}},
		"status": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("all"), formatFlag()}},
		"result": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("f"), boolFlag("events"), flag("save"),
			flag("head"), boolFlag("verdict"), flag("lines"), formatFlag()}},
		"cancel": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("lost"), boolFlag("dry-run"), formatFlag()}},
		"wait":   {Flags: []helptext.Flag{flag("timeout"), formatFlag()}},
		"brief":  {Flags: []helptext.Flag{flag("task"), flag("own"), boolFlag("read-only"), flag("report")}},
	}}
}

// agentSpec is the completion subtree builder for one agent verb (codex, copilot, opencode, gemini).
func agentSpec(name string) func() *helptext.Node {
	return func() *helptext.Node { return agentNode(name) }
}

func agentNode(name string) *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ask": {Flags: ask.CompletionFlags()},
		"review": {Flags: []helptext.Flag{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric"), flag("model")}},
		"explore":  {Flags: []helptext.Flag{boolFlag("no-web"), boolFlag("background"), formatFlag()}},
		"transfer": {Flags: []helptext.Flag{flag("source"), boolFlag("wait")}},
		"profile":  {Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{boolFlag("clear"), boolFlag("off")}},
		"setup":    {Flags: []helptext.Flag{formatFlag()}},
		"catalog":  catalogSpec(name),
	}}
}

// agentsPage is `agents -h`, also printed by bare `agents`.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "Run, track and list the agents (claude, codex, copilot, gemini, opencode), " +
		"and sync the roster into their homes.",
	Commands: [][2]string{
		{"list", "list each agent, its availability and honored fields; --format json"},
		{"ask", "one-shot task on an agent; --format json = envelope"},
		{"dispatch", "run a plan's waves: dispatch --waves <plan>"},
		{"sync", "project agents/ + CLAUDE.md into every agent home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"label", "print a leaf's dispatch description prefix"},
		{"find", "search repo content, paths, Go declarations or callers"},
		{"diff", "changed files, --path scope, Go gates; --check, --verify"},
		{"status", "dispatch state: status [<id>] [--agent A] [--all]"},
		{"result", "a dispatch's answer: result [<id>] [--agent A] [-f]"},
		{"cancel", "interrupt a running dispatch, or --lost: mark dead ones as error"},
		{"wait", "block until dispatches finish: wait <id>... [--timeout 10m]"},
		{"brief", "render a leaf brief: the task plus the contract footer"},
		{"send", "send one turn to a live claude or gemini session: send <id> <task>"},
	},
	Hint: "claude-code agents <command> -h lists that command's flags.",
}

// agentsSubPages are the `agents <verb> -h` pages: usage tail, blurb and hint.
var agentsSubPages = map[string][3]string{
	"find": {"find <pattern> [flags]", "Search tracked and new unignored files in path and line order; skip binary and >1 MiB files. " +
		"An unreadable or unparseable file is skipped and named on stderr. Callers are name-based. More than budget is a lower bound.",
		"Exit 0 match, 1 no match, 2 usage."},
	"list": {"list [flags]", "List each agent, whether it is available and which request fields (model, profile, effort, …) it honors."},
	"sync": {"sync [flags]", "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills " +
		"(plus gemini's and copilot's own skills dirs)."},
	"tree": {"tree [flags]", "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--finished adds idle and finished work in the --days window.",
		"Exit 0, 2 usage (bad --format, --watch with json, or a non-positive\n--interval)."},
	"label": {"label <type> [flags]", "Print the \"<model>: \" prefix the dispatch-effort hook requires on a dispatch " +
		"description, for a leaf of <type> with the given model and task.",
		"Exit 0, 1 the hook refuses <type> or the task is unreadable, 2 usage."},
	"diff": {"diff [flags]", "List the files changed since --base (default HEAD: uncommitted), with line counts, " +
		"the ones outside every --path, and with --check the Go and docs gates (on the --path files; complexity only for " +
		"findings the change introduced). --verify <id> adds a dispatch's routing proof. Exit 1 gate failed or " +
		"MISMATCH, 3 outside --path, 2 usage."},
	"send": {"send <id> <task | -> [flags]", "Send one user turn to a live claude or gemini session (ask --agent <a> " +
		"--live) and stream that turn's events; exits on its result. One turn at a time per session."},
	"dispatch": {"dispatch --waves <plan> [flags]", "Run a plan’s waves with observed verification and no commits; " +
		"the shared route and permission flags reach every leaf.",
		"Exit 0 passed, 1 failed wave, 2 usage or invalid plan,\n" +
			"3 outside owned files or unattributed, 4 interrupted."},
	"status": {"status [<id>] [flags]", "With no <id> and no --agent, list the 20 most recent dispatches of every agent. " +
		"With --agent, list that agent's jobs in this repo, newest first (--all: every repo). " +
		"An <id> shows that dispatch, whichever agent ran it.",
		"Exit 0, 1 unknown id or agent, 2 usage (--all without --agent)."},
	"result": {"result [<id>] [flags]", "Print a dispatch's log, its ask line and resume command; --format json prints " +
		"its envelope. With --agent and no <id>, the newest finished job of that agent in this repo. " +
		"-f follows a running job until it finishes, with a heartbeat on silence; --events streams its " +
		"events.jsonl instead of the log. --save <file> writes the text there and prints one summary line " +
		"(--head N echoes its first N lines). --verdict prints one status line and the last --lines N " +
		"(default 10) non-empty answer lines.",
		"Exit 0 ok, 1 errored or lost job, 2 usage (--save or --verdict without an\n" +
			"<id> or with -f or --events; --events with --format json; no <id> and\nno --agent)."},
	"cancel": {"cancel (<id> | --lost) [flags]", "Interrupt a running background dispatch, whichever agent runs it; " +
		"with --agent and no <id>, that agent's single running job in this repo. --lost instead marks " +
		"dispatch records still \"running\" whose process is gone as error and drops stale agent-inflight " +
		"markers; --dry-run prints what it would change and writes nothing.",
		"Exit 0, 1 failed, 2 usage (--lost with an <id>, --agent or --format json;\n" +
			"--dry-run without --lost; none of <id>, --agent and --lost)."},
	"wait": {"wait <id>... [flags]", "Block until every dispatch has finished and print one row per id: <id> done|error|lost|timeout <elapsed>.",
		"Exit 0 all done, 1 any error or lost, 2 usage, 4 timed out, still running."},
	"brief": {"brief --task <file|-> [--own <path>]... [flags]", "Print a leaf brief: an Owned: line, the mode, the task, the leaf rules and the " +
		"reporting contract. Pipe it to agents ask -; agents diff --verify <id> reads the Owned: line back.",
		"Exit 0, 2 usage (no --task, or an implementation brief with no --own)."},
}

// agentsFlags makes the `agents <verb>` FlagSet whose -h renders its page.
func agentsFlags(verb string) *flag.FlagSet {
	sp := agentsSubPages[verb]
	return helptext.NewFlagSet("agents "+verb, helptext.Page{Usage: "claude-code agents " + sp[0], About: sp[1], Hint: sp[2]})
}

// cmdAgents is `claude-code agents`.
func cmdAgents(args []string) int {
	if code, ok := helpOrBare(agentsPage, args); ok {
		return code
	}
	if run := agentsVerbs[args[0]]; run != nil {
		return run(args[1:])
	}
	return unknownSub("agents", args[0], agentsPage)
}

// cmdAgentsAsk runs ask after starting a silent, unwaited reap, so orphaned dispatch children never pile up unseen.
func cmdAgentsAsk(args []string) int {
	go tree.Reap(false, io.Discard, io.Discard)
	return ask.Cmd(args)
}

// agentsVerbs routes `agents <verb>` to its handler.
var agentsVerbs = map[string]func([]string) int{
	"list": cmdAgentsList, "ask": cmdAgentsAsk, "dispatch": cmdAgentsDispatch, "sync": cmdAgentsSync,
	"tree": cmdAgentsTree, "send": cmdAgentsSend, "label": cmdAgentsLabel,
	"diff":   cmdAgentsDiff,
	"find":   cmdAgentsFind,
	"status": cmdAgentsStatus,
	"result": cmdAgentsResult,
	"cancel": cmdAgentsCancel,
	"wait":   cmdAgentsWait,
	"brief":  cmdAgentsBrief,
}

// cmdAgentsList is `agents list`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := agentsFlags("list")
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents list --format)")
	format := fs.String("format", "text", "text, or json: caps, events, permissions")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents list", err)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if *host != "" {
		if err := agent.ListRemote(*host, remote.Bin(), *format, os.Stdout); err != nil {
			return helptext.Fail("%v", err)
		}
		return 0
	}
	return printRoster(*format)
}

// cmdAgentsSync renders agents/ into state/projections, installs the copies into agent homes and links skills/.
func cmdAgentsSync(args []string) int {
	fs := agentsFlags("sync")
	name := fs.String("agent", "", "one target: "+strings.Join(project.Names(), "|")+" (default: all)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	format := fs.String("format", "text", "output: text|json (json = one row per destination)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents sync", err)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return project.Sync(project.Options{JSON: *format == "json", DryRun: *dryRun, Check: *check, Agent: *name, Out: os.Stdout})
}

// cmdAgentsTree is `agents tree`; --watch clears and redraws every --interval until a render fails.
func cmdAgentsTree(args []string) int {
	fs := agentsFlags("tree")
	days := fs.Int("days", 1, "sessions modified in the last N days (ignored with --session)")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	finished := fs.Bool("finished", false, "also show idle and finished work")
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
	opts := tree.Options{Days: *days, Session: *session, Running: !*finished && *session == ""}
	return tree.Show(paths.ConfigDir(), opts, tree.View{JSON: *format == helptext.JSON, All: *finished || *session != "",
		Watch: *watch, Interval: *interval})
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

// cmdAgentsLabel is `agents label <type>`: the description prefix the hook would write for this dispatch.
func cmdAgentsLabel(args []string) int {
	fs := agentsFlags("label")
	model := fs.String("model", "", "the dispatch's model (default unset: the hook classifies the brief)")
	taskFile := fs.String("task", "", "the task file the leaf gets; - reads stdin")
	sessionModel := fs.String("session-model", "", "the session's model id (default: from this session's transcript)")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents label", err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	task, err := hookagents.ReadBrief(*taskFile)
	if err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	row := hookagents.Label(hookagents.LabelInput{Type: pos[0], Model: *model, Brief: task, SessionModel: *sessionModel},
		cwd, os.Getenv(memory.SessionIDEnv))
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents label", row)
	}
	if row.Blocked != "" { // the hook refuses this dispatch: say so instead of printing a prefix it never accepts
		return helptext.Fail("agents label: %s", row.Blocked)
	}
	if row.Label != "" { // an empty label is no prefix, as the hook writes none
		if _, err := fmt.Println(row.Label + ": "); err != nil {
			return helptext.Fail("agents label: %v", err)
		}
	}
	return 0
}

// cmdAgentsDiff is `agents diff`: changed files, ownership and, with --check, the Go gates.
func cmdAgentsDiff(args []string) int {
	fs := agentsFlags("diff")
	base := fs.String("base", "", "compare the working tree against this ref (default HEAD)")
	var sel []string
	fs.Func("path", "a path or glob the change may touch; repeat per path; --check covers only these", func(v string) error {
		sel = append(sel, v)
		return nil
	})
	verify := fs.String("verify", "", "also prove where dispatch <id> ran: workspace, model, fallback, proxy requests")
	check := fs.Bool("check", false, "run gofmt, build, vet, lint, complexity, test and docs on the touched packages")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents diff", err)
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents diff: %v", err)
	}
	return agent.Diff(agent.DiffOptions{Dir: cwd, Base: *base, Paths: sel, Verify: *verify, Check: *check, JSON: *format == helptext.JSON,
		Out: os.Stdout, Err: os.Stderr})
}

func cmdAgentsFind(args []string) int {
	fs := agentsFlags("find")
	o := agent.FindOptions{Dir: ".", Out: os.Stdout, Err: os.Stderr}
	fs.BoolVar(&o.Name, "name", false, "match file paths instead of content")
	fs.BoolVar(&o.Func, "func", false, "match Go function or method declarations")
	fs.BoolVar(&o.Def, "def", false, "match Go top-level func, type, const or var declarations")
	fs.BoolVar(&o.Callers, "callers", false, "name-based Go calls to exactly pattern; no type resolution")
	fs.BoolVar(&o.IgnoreCase, "i", false, "ignore case")
	fs.BoolVar(&o.Word, "w", false, "match whole words")
	fs.BoolVar(&o.Files, "l", false, "print matching file names only")
	fs.IntVar(&o.Max, "max", agent.FindDefaultMax, "result-line budget; more is a lower bound when cut")
	fs.Func("path", "limit to a repo path prefix or glob; repeat per path (as agents diff)", func(v string) error {
		o.Paths = append(o.Paths, v)
		return nil
	})
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents find", err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	o.Pattern, o.JSON = pos[0], *format == helptext.JSON
	return agent.Find(o)
}

// cmdAgentsDispatch is `agents dispatch --waves <plan>`: the wave runner.
func cmdAgentsDispatch(args []string) int {
	fs := agentsFlags("dispatch")
	timeout := fs.Duration("timeout", 0, "per-dispatch time limit (0 = the agent's default)")
	format := helptext.FormatFlag(fs)
	background := fs.Bool("background", false, "detach the run, print its id")
	route := ask.RegisterRouteFlags(fs)
	waveOpts := registerDispatchWaves(fs)
	job := fs.String("job", "", "internal detached wave dispatch id")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("dispatch", err)
	}
	if waveOpts.Plan == "" {
		return helptext.FailUsage("agents dispatch: --waves <plan> is required")
	}
	waveOpts.Timeout, waveOpts.JSON = *timeout, *format == helptext.JSON
	return dispatchWaves(pos, route, waveOpts, waveJob{*background, *job, args})
}

// cmdAgentsWait is `agents wait <id>...`: one row per dispatch once all have settled; exit 1 on error or lost, 4 on timeout.
func cmdAgentsWait(args []string) int {
	fs := agentsFlags("wait")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up on unfinished dispatches after this long (state timeout)")
	format := helptext.FormatFlag(fs)
	ids, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents wait", err)
	}
	if len(ids) == 0 {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rows, err := agent.Wait(ctx, ids, *timeout, time.Second)
	if err != nil {
		return helptext.Fail("agents wait: %v", err)
	}
	if *format == helptext.JSON {
		if code := helptext.WriteJSONOrFail("agents wait", rows); code != 0 {
			return code
		}
	} else {
		for _, r := range rows {
			fmt.Println(r.Line())
		}
	}
	return agent.WaitExit(rows)
}

// cmdAgentsBrief is `agents brief`: the rendered brief on stdout; an implementation brief needs --own.
func cmdAgentsBrief(args []string) int {
	fs := agentsFlags("brief")
	taskFile := fs.String("task", "", "the authored task text file; - reads stdin")
	var owned []string
	fs.Func("own", "a file or directory the leaf may edit; repeat per path", func(v string) error {
		owned = append(owned, v)
		return nil
	})
	readOnly := fs.Bool("read-only", false, "the leaf edits nothing (no --own needed)")
	report := fs.String("report", "", "where the leaf writes its full report")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *taskFile == "" || len(pos) > 0 || (!*readOnly && len(owned) == 0) {
		fs.Usage()
		return 2
	}
	task, err := hookagents.ReadBrief(*taskFile)
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	if strings.TrimSpace(task) == "" {
		return helptext.Fail("agents brief: --task %q is empty", *taskFile)
	}
	out, err := agent.RenderBrief(agent.BriefInput{Task: task, Owned: owned, ReadOnly: *readOnly, Report: *report})
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	if _, err := fmt.Print(out); err != nil {
		return helptext.Fail("agents brief: write output: %v", err)
	}
	return 0
}

// cmdAgentsSend is `agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := agentsFlags("send")
	format := fs.String("format", helptext.Text, "stream text, print the JSON envelope, or stream-json NDJSON events")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains(agent.Formats, *format) {
		fs.Usage()
		return 2
	}
	return ask.Send(pos[0], strings.Join(pos[1:], " "), *format)
}

// agentAbout expands each agent's one-line menu summary for its help page.
var agentAbout = map[string]string{
	"copilot": "GitHub's Copilot CLI on its own login, or the selected Databricks profile. Bare " +
		"launches it with args forwarded; the commands below run it headless, in the background or as a reviewer.",
	"codex": "OpenAI's Codex CLI on your ChatGPT login, the second-opinion reviewer. Bare " +
		"launches it with args forwarded; the commands below run it headless.",
	"gemini": "Gemini through agy (the Antigravity CLI) on your Google sign-in. Bare launches it " +
		"with args forwarded; the commands below run it headless (ask --dirs packs files).",
	"opencode": "The OpenCode CLI, a general code agent with the widest model pool (catalog), on its " +
		"own login or the selected Databricks profile. Bare launches it with args forwarded; the " +
		"commands below run it headless.",
}

// agentPage is the help page for one agent verb: the shared command roster plus the CLI's extra rows.
func agentPage(name string) helptext.Page {
	rows := [][2]string{
		{"ask", "one-shot run (= ask --agent " + name + ")"},
		{"review", "diff review; --adversarial"},
		{"explore", "scan code + web research → brief"},
		{"transfer", "hand this Claude session to " + name + "; --source"},
		{"setup", "binary, login, route, projection; --format json"},
		{"catalog", "model catalog; on Databricks: list|check|sync"},
		{"profile", "show or set " + name + "'s profile; --clear = own login; --off"},
	}
	if exploreRefused(name) {
		rows = slices.DeleteFunc(rows, func(r [2]string) bool { return r[0] == "explore" })
	}
	s, _ := agent.LookupSchema(name)
	catalogRow := slices.IndexFunc(rows, func(r [2]string) bool { return r[0] == "catalog" })
	switch {
	case s.CLI.CatalogUsage != "":
		rows[catalogRow][1] = s.CLI.CatalogUsage
	case len(s.CLI.Dialects) == 0:
		rows[catalogRow][1] = "own model list; no Databricks route"
	}
	rows = append(rows, s.CLI.ExtraUsage...)
	binary := name
	if s.External() {
		binary = s.CLI.Binary
	}
	return helptext.Page{
		Usage:    "claude-code " + name + " [<command>|--] [launch flags] [args…]",
		About:    agentAbout[name],
		Commands: rows,
		Flags:    launchFlags(name),
		Hint: "claude-code " + name + " <command> -h lists that command's flags.\n" +
			"claude-code " + name + " " + helptext.DocsFlag + " prints " + binary + "'s own help.",
	}
}

// exploreRefused reports whether `<agent> explore` is refused: it reads with tools but cannot enforce read-only.
func exploreRefused(name string) bool {
	s, ok := agent.LookupSchema(name)
	return ok && s.CLI.ReadNeedsTools && s.CLI.Caps.ReadOnly != agent.Native
}

// launchFlags lists the harness flags a bare launch consumes (agent.ParseArgs) for the help page.
func launchFlags(name string) *flag.FlagSet {
	fs := helptext.NewFlagSet(name, helptext.Page{Usage: "claude-code " + name}) // display only; ParseArgs parses
	fs.String("model", "", "select the model for this launch")
	profileUsage := "harness profile for this launch"
	if name == "codex" {
		profileUsage += " (codex's own: -p NAME)"
	}
	helptext.NameVar(fs, new(string), "profile", profileUsage)
	fs.Bool("direct", false, "skip the proxy; hand the CLI the profile route")
	fs.Bool("print-env", false, "print the resolved binary, argv and env; do not launch")
	fs.String("overlay", "", "env layer {set,unset,keep}: file, inline JSON, or -")
	fs.String("keep-env", "", "env names the credential scrub keeps")
	if name == "codex" {
		fs.String("file", "", "config.toml whose keys ride as -c overrides")
		fs.String("f", "", "shorthand for --file")
	}
	return fs
}

// agentSubPages maps each sub-verb to its synopsis tail and blurb; %s is the agent name.
var agentSubPages = map[string][2]string{
	"setup":   {"setup [flags]", "Check %s's binary, login, route and projection; exits 1 if binary or login fails."},
	"catalog": {"catalog [list|check|sync] [flags]", "Databricks serving endpoints %s can use, or its own model catalog."},
	"review":  {"review [flags] [focus…]", "Review a diff with %s; trailing words name the focus."},
	"explore": {"explore [flags] <question>", "Scan the code and research the web with %s, then return a brief."},
	"profile": {"profile [<profile>|--clear|--off]", "Show %s's profile, or set %[1]s to <profile>; --clear = own login, " +
		"--off = no profile (stop routing). The three are exclusive. Exit 0, 1 failed."},
	"transfer": {"transfer [flags] [note…]", "Hand this session's transcript to %s with tools on; detached unless --wait."},
}

// agentSubPage is the help page for `<agent> <verb>`.
func agentSubPage(name, verb string) helptext.Page {
	sp := agentSubPages[verb]
	about := sp[1]
	if strings.Contains(about, "%") {
		about = fmt.Sprintf(about, name)
	}
	if verb == "explore" && exploreRefused(name) {
		about = "Refused on " + name + " (read-only by prompt only); use codex or copilot."
	}
	return helptext.Page{Usage: "claude-code " + name + " " + sp[0], About: about}
}

// newAgentFlags makes a ContinueOnError FlagSet named "<agent> <verb>" whose -h renders the sub-verb's page.
func newAgentFlags(name, verb string) *flag.FlagSet {
	return helptext.NewFlagSet(name+" "+verb, agentSubPage(name, verb))
}

// retiredAgentVerbs are refused, not forwarded to the agent CLI; their jobs moved to agents verbs and ask --session.
var retiredAgentVerbs = []string{"resume", "status", "tail", "result", "cancel", "on", "off"}

// cmdAgentVerb is `<agent>`: bare or `--` launches, `ask` = `ask --agent <v>`, a sub-verb runs it, else launch.
func cmdAgentVerb(name string, args []string) int {
	switch {
	case len(args) == 0:
		return ask.Launch(name, args)
	case args[0] == "--":
		return ask.Launch(name, args[1:])
	case slices.Contains(retiredAgentVerbs, args[0]):
		return unknownSub(name, args[0], agentPage(name))
	case args[0] == "ask":
		return ask.Run(append([]string{"--agent", name}, args[1:]...), name)
	case agentSubPages[args[0]] != [2]string{}:
		return agentSub(name, args)
	case len(args) == 1 && isHelpVerb(args[0]):
		agentPage(name).Fprint(os.Stderr)
		return 0
	case len(args) == 1 && args[0] == helptext.DocsFlag:
		s, _ := agent.LookupSchema(name)
		return helptext.WrappedDocs(s.CLI.Binary)
	}
	return ask.Launch(name, args)
}

// agentSub dispatches an agent's subverbs (setup, catalog, explore, transfer, profile, review).
func agentSub(name string, args []string) int {
	handlers := map[string]func(string, []string) int{
		"setup": agentSetup, "catalog": agentCatalog, "explore": agentExplore,
		"transfer": agentTransfer, "profile": agentProfile, "review": agentReview,
	}
	if run := handlers[args[0]]; run != nil {
		return run(name, args[1:])
	}
	return helptext.Fail("%s: unknown subcommand %q", name, args[0])
}

// agentSetup is `<v> setup [--format text|json]`.
func agentSetup(name string, args []string) int {
	fs := newAgentFlags(name, "setup")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" setup", err)
	}
	return ask.Setup(name, *format)
}

// agentReview is `<v> review [scope flags] [--adversarial] [--background] [--rubric f] [focus…]`.
func agentReview(name string, args []string) int {
	fs := newAgentFlags(name, "review")
	var o ask.ReviewOptions
	fs.BoolVar(&o.Uncommitted, "uncommitted", false, "working tree + index vs HEAD (default)")
	fs.StringVar(&o.Base, "base", "", "diff against this branch (triple-dot merge base)")
	fs.StringVar(&o.Commit, "commit", "", "diff this single commit")
	fs.BoolVar(&o.PR, "pr", false, "diff against the remote's default branch")
	fs.BoolVar(&o.Adversarial, "adversarial", false, "adversarial findings instead of a neutral verdict")
	fs.BoolVar(&o.Background, "background", false, "detach; claude-code agents status|result --agent "+name+" to follow up")
	fs.StringVar(&o.Rubric, "rubric", "", "grade the review against this rubric file")
	fs.StringVar(&o.Model, "model", "", "model for the review (default: the agent's pin)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	o.Focus = strings.Join(fs.Args(), " ")
	return ask.Review(name, args, o)
}

// agentExplore is `<v> explore [--no-web] [--background] [--format f] <question>`.
func agentExplore(name string, args []string) int {
	fs := newAgentFlags(name, "explore")
	noWeb := fs.Bool("no-web", false, "skip the web-research step; code scan only")
	background := fs.Bool("background", false, "detach; claude-code agents status|result --agent "+name+" to follow up")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	question := strings.Join(fs.Args(), " ")
	if question == "" {
		return helptext.Fail("%s explore: usage: claude-code %s explore [--no-web] [--background] <question>", name, name)
	}
	return ask.Explore(name, question, !*noWeb, *background, *format)
}

// agentTransfer is `<v> transfer [--source f] [--wait] [note…]`.
func agentTransfer(name string, args []string) int {
	fs := newAgentFlags(name, "transfer")
	source := fs.String("source", "", "transcript .jsonl (default: newest session for this repo)")
	wait := fs.Bool("wait", false, "block for the answer instead of detaching")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	return ask.Transfer(name, *source, strings.Join(fs.Args(), " "), *wait)
}

// firstPositional is pos[0], or "" when there is none.
func firstPositional(pos []string) string {
	if len(pos) == 0 {
		return ""
	}
	return pos[0]
}

// agentProfile is `<v> profile [<profile>|--clear|--off]`: show, set, clear or switch off the agent's record profile.
func agentProfile(name string, args []string) int {
	if _, ok := agent.LookupSchema(name); !ok {
		return helptext.Fail("%s profile: unknown agent", name)
	}
	fs := newAgentFlags(name, "profile")
	unpin := fs.Bool("clear", false, "reset to its own login ("+vault.OwnLoginProfiles[name]+")")
	off := fs.Bool("off", false, "clear the record's profile: stop routing to "+name)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && *off) || ((*unpin || *off) && len(pos) > 0) {
		return helptext.Fail("%s profile: want one profile name, --clear or --off", name)
	}
	if err := configure.AgentProfile(os.Stdout, name, firstPositional(pos), *unpin, *off); err != nil {
		return helptext.Fail("%s profile: %v", name, err)
	}
	return 0
}

// printRoster prints agent.Summaries as the JSON array of rows, or one text line each.
func printRoster(format string) int {
	rows := agent.Summaries()
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents", rows)
	}
	for _, r := range rows {
		fmt.Println(r.Line())
	}
	return 0
}
