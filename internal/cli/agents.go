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
	"github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/agent/tree"
	resolve "github.com/edotau/claude-code/internal/cli/gatewayresolve"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/dev/configure"
	"github.com/edotau/claude-code/internal/hooks/dispatchhook"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/rtkwrap"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentconfig/project"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agentsSpec is the completion node for the agents verb.
func agentsSpec() *helptext.Node {
	flag, boolFlag, valuedFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"sync": {Flags: []helptext.Flag{valuedFlag("agent", project.Names), boolFlag("dry-run"), boolFlag("check"),
			formatFlag()}},
		"dashboard": {Flags: []helptext.Flag{flag("days"), flag("session"), boolFlag("detail"), flag("interval"),
			boolFlag("files")}},
		"tree": {Flags: []helptext.Flag{flag("days"), flag("session"), boolFlag("detail"),
			boolFlag("watch"), flag("interval"), boolFlag("files"), formatFlag()}},
		"codemode": {Flags: []helptext.Flag{flag("session")}},
		"label":    {Flags: []helptext.Flag{flag("model"), flag("profile"), flag("task"), flag("session-model"), formatFlag()}},
		"find": {Flags: []helptext.Flag{boolFlag("name"), boolFlag("func"), boolFlag("def"), boolFlag("callers"),
			boolFlag("i"), boolFlag("w"), boolFlag("l"), flag("path"), flag("max"), formatFlag()}},
		"diff": {Flags: []helptext.Flag{flag("base"), flag("path"), flag("verify"), boolFlag("check"), valuedFlag("gates", inspect.GateNames),
			boolFlag("all"), boolFlag("batch"), formatFlag()}},
		"fix":    {Flags: []helptext.Flag{flag("path"), boolFlag("dry-run")}},
		"list":   {Flags: []helptext.Flag{flag("host"), formatFlag()}},
		"ask":    {Flags: ask.CompletionFlags()},
		"send":   {Flags: []helptext.Flag{flag("format"), flag("timeout")}},
		"msg":    {Flags: []helptext.Flag{flag("from"), flag("file"), flag("format")}},
		"mcp":    {Flags: []helptext.Flag{flag("from")}},
		"status": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("all"), formatFlag()}},
		"result": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("f"), boolFlag("events"), flag("after"), flag("save"),
			flag("head"), boolFlag("verdict"), flag("lines"), formatFlag()}},
		"cancel": {Flags: []helptext.Flag{valuedFlag("agent", agent.RosterNames), boolFlag("lost"), boolFlag("dry-run"), formatFlag()}},
		"wait":   {Flags: []helptext.Flag{flag("timeout"), boolFlag("progress"), formatFlag()}},
		"brief":  {Flags: []helptext.Flag{flag("task"), flag("own"), flag("report"), flag("agent"), flag("verify"), flag("plan-task"), flag("shape"), flag("name"), flag("description"), flag("trigger"), flag("non-trigger"), flag("task-case"), flag("not-for"), flag("reference"), flag("from"), boolFlag("synthesis-schema"), boolFlag("write"), valuedFlag("tier", agent.TierNames), valuedFlag("codemode", func() []string { return []string{"true", "false"} }), boolFlag("path")}},
	}}
}

// agentSpec is the completion subtree builder for one agent verb (codex, copilot, opencode, gemini).
func agentSpec(name string) func() *helptext.Node {
	return func() *helptext.Node { return agentNode(name) }
}

func agentNode(name string) *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	n := &helptext.Node{Sub: map[string]*helptext.Node{
		"review": {Flags: []helptext.Flag{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric"), flag("model"), flag("overlay"), formatFlag()}},
		"transfer": {Flags: []helptext.Flag{flag("source"), boolFlag("wait")}},
		"profile":  {Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{boolFlag("clear"), boolFlag("off")}},
		"catalog":  catalogSpec(name),
	}}
	if name == "gemini" {
		n.Sub["vscode"] = &helptext.Node{Flags: []helptext.Flag{flag("profile"), flag("overlay"), flag("model"),
			flag("keep-env"), boolFlag("direct"), boolFlag("print-env")}}
	}
	if name == "codex" {
		task := func(f ...helptext.Flag) *helptext.Node { return &helptext.Node{Flags: append(f, flag("overlay"))} }
		n.Sub["cloud"] = &helptext.Node{Sub: map[string]*helptext.Node{
			"exec": task(flag("env"), flag("attempts"), flag("branch")), "status": task(),
			"list": task(flag("env"), flag("limit")), "diff": task(flag("attempt")), "apply": task(flag("attempt")),
		}}
	}
	return n
}

// agentsPage is `agents -h`, also printed by bare `agents`.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "Run, track and list the agents (claude, codex, copilot, gemini, opencode), " +
		"and sync the roster into their homes.",
	Commands: [][2]string{
		{"list", "list each agent, its availability and honored fields; --format json"},
		{"ask", "one-shot task on an agent; --format json = envelope"},
		{"sync", "project agents/ + CLAUDE.md into every agent home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"dashboard", "walk the live tree: move, expand, collapse; enter follows a job"},
		{"label", "print a leaf's dispatch description prefix"},
		{"codemode", "codemode on|off|status: this session's rtk-free shell"},
		{"find", "search repo content, paths, Go declarations or callers"},
		{"diff", "changed files, --path scope, Go gates; --check, --verify"},
		{"fix", "run the make format fixers on --path files only; --dry-run"},
		{"status", "dispatch state: status [<id>] [--agent A] [--all]"},
		{"result", "a dispatch's answer: result [<id>] [--agent A] [-f]"},
		{"cancel", "interrupt a running dispatch, or --lost: mark dead ones as error"},
		{"wait", "block until dispatches finish: wait <id>... [--timeout 9m30s]"},
		{"watch", "report this session's stale dispatches: watch [--once]"},
		{"brief", "render a leaf brief or reserve its file with --path"},
		{"send", "send a turn to a live session, or resume a finished job"},
		{"msg", "message an agent or chat: msg @name \"text\"; bare reads new"},
		{"mcp", "serve msg, gate and diff tools as a stdio MCP server"},
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
		"(plus gemini's and copilot's own skills dirs). Also re-renders each agent's native hook and config files " +
		"(codex, opencode, gemini, copilot); --check reports a stale one."},
	"tree": {"tree [flags]", "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--detail adds idle, stale and finished work in the --days window, and each dispatch's task and id line. Subagent columns: marker, agent, model, " +
		"↑tokens in, ↓tokens out, Σ subtree total, age, idle; piped output is unclipped, a terminal clips rows to 80 columns. " +
		"--files adds each leaf's still-dirty files (claims ledger, or a dispatch's run), its latest undelivered note, and the dirty files no live leaf holds.",
		"Exit 0, 2 usage (bad --format, --watch with json, or a non-positive --interval)."},
	"codemode": {"codemode on|off|status [flags]", "Turn this Claude session's code mode on (Bash calls are not " +
		"rewritten to rtk) or off, or print its state. Dispatched children keep their own --codemode.",
		"Prints codemode: on|off (<source>). Exit 0, 2 usage."},
	"label": {"label <type> [flags]", "Print the \"<model>: \" prefix the dispatch-effort hook requires on a dispatch " +
		"description, for a leaf of <type> with the given model and task.",
		"Exit 0, 1 the hook refuses <type> or the task is unreadable, 2 usage."},
	"diff": {"diff [flags]", "List the files changed since --base (default HEAD: uncommitted), with line counts, " +
		"the ones outside every --path, each new Go test or helper with its caller count (advisory), and with --check the Go and docs gates (on the --path files; complexity only for " +
		"findings the change introduced), findings as `path:line: tool/rule: message` rows. --gates picks gates (lint: " +
		"gofmt, vet, golangci-lint, docs, yaml, shell, quality; a missing golangci-lint or mdsmith then fails); --all checks the whole " +
		"tree. --batch merges this session's queued handbacks and clears them after a full pass. --verify <id> adds a dispatch's routing proof and flags pre-existing dirty files it touched outside --path. Exit 1 gate failed or MISMATCH, 3 outside --path, 2 usage."},
	"fix": {"fix --path <p> [flags]", "Run the fixers make format runs (gofmt, golangci-lint fmt, mdsmith fix, yamlfmt) on " +
		"the tracked files under each --path, and nothing else. Refuses no --path, an untracked or symlinked file, and a " +
		"file a live session or leaf holds. --dry-run lists what would change.",
		"Exit 0 fixed or clean, 1 skipped file, missing tool or failed fixer, 2 usage."},
	"send": {"send <id> <task | -> [flags]", "Send one user turn to a live claude or gemini session (ask --agent <a> " +
		"--live), or wait for a running dispatch then resume its session on any agent as a new child dispatch; exits on its result. One turn at a time per live session."},
	"msg": {"msg [@name|'#name'] [<text | ->] [flags]", "Message one agent or Claude session (@gemini, @claude-79) or a " +
		"named room ('#design'), and read what was sent. With text (or --file) it sends to the target; with a target " +
		"and no text it prints that chat's new messages; bare it prints every new message for you. Every message " +
		"shows its sender. Quote '#name': a bare # starts a shell comment. A live Claude member also gets each " +
		"message as a notice on its next hook. msg @parent <note> is a leaf's one-line note (200 characters, literal " +
		"text) to its dispatcher: exit 0 with \"no dispatcher to notify\" on stderr when none is live; it skips a " +
		"repeat and refuses a sixth unread note. " +
		"Exit 0, 1 failed, 2 usage."},
	"mcp": {"mcp --from <agent>", "Serve the harness tools over stdio MCP as one server: msg_send and msg_read " +
		"(the sender is --from, never a tool argument), gate and diff (typed arguments; Go builds the command). " +
		"Exit 0, 1 failed, 2 usage."},
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
	"brief": {"brief [--path|--task <file|->|--shape <id>] [flags]",
		"Render a leaf brief, or --path: reserve an empty file and print its path.\n" +
			"Path mode takes only an optional agent; files expire after seven days.\n" +
			"--shape skill: print SKILL.md and evals.md; --write creates skills/<name>/.",
		"Exit 0, 1 I/O error, 2 usage."},
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

// reapLost is the silent reap before an ask (tests swap it).
var reapLost = func() { tree.Reap(false, io.Discard, io.Discard) }

// cmdAgentsAsk is `agents ask`; ask calls the reap hook for a real run and joins it at exit.
func cmdAgentsAsk(args []string) int { return ask.Cmd(args) }

func wireAsk() {
	ask.Waves = cmdAgentsWaves
	ask.Reap = func() func() {
		done := make(chan struct{})
		parallel.Go(func() {
			reapLost()
			close(done)
		}, func(err error) { fmt.Fprintln(os.Stderr, "agents reap:", err); close(done) })
		return func() { <-done }
	}
}

// agentsVerbs routes `agents <verb>` to its handler.
var agentsVerbs = map[string]func([]string) int{
	"list": cmdAgentsList, "ask": cmdAgentsAsk, "sync": cmdAgentsSync,
	"tree": cmdAgentsTree, "send": cmdAgentsSend, "msg": cmdAgentsMsg, "mcp": cmdAgentsMcp, "label": cmdAgentsLabel,
	"codemode":  cmdAgentsCodemode,
	"dashboard": cmdAgentsDashboard,
	"diff":      cmdAgentsDiff,
	"fix":       cmdAgentsFix,
	"find":      cmdAgentsFind,
	"status":    cmdAgentsStatus,
	"result":    cmdAgentsResult,
	"cancel":    cmdAgentsCancel,
	"wait":      cmdAgentsWait,
	"brief":     cmdAgentsBrief,
	"watch":     cmdAgentsWatch,
}

// cmdAgentsList is `agents list`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := agentsFlags("list")
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents list --format)")
	format := helptext.FormatFlagWithUsage(fs, "text, or json: caps, events, permissions")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	if *host != "" {
		if err := agent.ListRemote(*host, *format, os.Stdout); err != nil {
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
	format := helptext.FormatFlagWithUsage(fs, "output: text|json (json = one row per destination)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	code := project.Sync(project.Options{JSON: *format == "json", DryRun: *dryRun, Check: *check, Agent: *name, Out: os.Stdout})
	if code == 2 {
		return code
	}
	if cfg := syncAgentConfigs(*name, *format, *dryRun, *check); cfg != 0 {
		return cfg
	}
	return code
}

// syncAgentConfigs re-renders the agents' native hook and config files, which project.Sync does not write.
func syncAgentConfigs(name, format string, dryRun, check bool) int {
	o := agentconfig.RenderOptions{DryRun: dryRun, Check: check}
	if format != "json" {
		o.Out = os.Stdout // SyncConfigs names the agents it skipped here
	}
	if name != "" {
		if !slices.Contains(agent.DefaultAgents, name) {
			return 0
		}
		o.Agents = []string{name}
	}
	changed, skipped, err := agent.SyncConfigs(o)
	if skipped {
		return 0
	}
	if format != "json" && len(changed) > 0 {
		verb := "WRITE"
		if dryRun || check {
			verb = "DRY"
		}
		fmt.Printf("\nagent configs\n  %s %s\n", verb, strings.Join(changed, " "))
	}
	return agentConfigsExit(changed, err, check)
}

// agentConfigsExit is 1 for a failed render, or for stale configs under --check.
func agentConfigsExit(changed []string, err error, check bool) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync: agent configs:", err)
		return 1
	}
	if check && len(changed) > 0 {
		fmt.Fprintf(os.Stderr, "stale: agent configs of %s; run `claude-code agents sync`\n", strings.Join(changed, ", "))
		return 1
	}
	return 0
}

// cmdAgentsTree is `agents tree`; --watch clears and redraws every --interval until a render fails.
func cmdAgentsTree(args []string) int {
	fs := agentsFlags("tree")
	days := fs.Int("days", 1, "last N days; ignored with --session")
	session := fs.String("session", "", "scope to one session id (every project dir is searched)")
	detail := fs.Bool("detail", false, "also show idle, stale, finished work; dispatch task and id")
	watch := fs.Bool("watch", false, "clear and re-render on --interval (text only)")
	interval := fs.Duration("interval", 2*time.Second, "redraw interval for --watch")
	files := fs.Bool("files", false, "show each leaf's dirty files and note, and the free files")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := checkTreeFlags(*format, *watch, *interval); err != nil {
		fmt.Fprintln(os.Stderr, "agents tree:", err)
		return 2
	}
	all := *detail || *session != "" // a named session shows whole; json always does
	opts := tree.Options{Days: *days, Session: *session, Files: *files}
	return tree.Show(paths.ConfigDir(), opts, tree.View{JSON: *format == helptext.JSON, All: all,
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
	profile := fs.String("profile", "", "the ask --profile the leaf runs on")
	taskFile := fs.String("task", "", "the task file the leaf gets; - reads stdin")
	sessionModel := fs.String("session-model", "", "the session's model id (default: from this session's transcript)")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	if err := refuseModelPrefix(*model); err != nil {
		return helptext.FailUsage("agents label: %v", err)
	}
	task, err := agent.ReadBrief(*taskFile)
	if err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	row, err := labelRowFor(dispatchhook.LabelInput{Type: pos[0], Model: *model, Profile: *profile, Brief: task,
		SessionModel: *sessionModel})
	if err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents label", row)
	}
	return emitLabel(row, *model, *profile, task)
}

// labelRowFor is the hook's label row for in.
func labelRowFor(in dispatchhook.LabelInput) (dispatchhook.LabelRow, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return dispatchhook.LabelRow{}, err
	}
	return dispatchhook.Label(in, cwd, os.Getenv(paths.SessionIDEnv)), nil
}

// emitLabel prints the label after the hook's refusal and notes.
func emitLabel(row dispatchhook.LabelRow, model, profile, task string) int {
	if row.Blocked != "" { // the hook refuses this dispatch: say so instead of printing a prefix it never accepts
		return helptext.Fail("agents label: %s", row.Blocked)
	}
	if row.Note != "" {
		fmt.Fprintln(os.Stderr, row.Note)
	}
	if row.Type == "gemini" {
		resolve.GoogleLabelWarning(context.Background(), os.Stderr, row.Type, model, profile, task)
	}
	return printLabel(row.Label)
}

// printLabel prints the description prefix; an empty label prints nothing.
func printLabel(label string) int {
	if label == "" {
		return 0
	}
	if !strings.HasSuffix(label, ":") { // a tier list already ends in its colons
		label += ": "
	}
	if _, err := fmt.Println(label); err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	return 0
}

// parseWithPaths parses args, taking bare words between flags as extra paths; a dash word is a one-line error.
func parseWithPaths(fs *flag.FlagSet, args []string, sel *[]string) (int, bool) {
	for {
		if code, done := helptext.Parse(fs, args); done {
			return code, true
		}
		args = fs.Args()
		if len(args) == 0 {
			return 0, false
		}
		if strings.HasPrefix(args[0], "-") && args[0] != "-" {
			return helptext.FailUsage("agents %s: unexpected argument %q", fs.Name(), args[0]), true
		}
		*sel = append(*sel, strings.Split(args[0], ",")...)
		args = args[1:]
	}
}

// cmdAgentsDiff is `agents diff`: changed files, ownership and, with --check, the Go gates.
func cmdAgentsDiff(args []string) int {
	fs := agentsFlags("diff")
	base := fs.String("base", "", "compare the working tree against this ref (default HEAD)")
	var sel []string
	fs.Func("path", "paths or globs the change may touch: a,b or repeat; trailing args add more; --check covers only these", func(v string) error {
		sel = append(sel, strings.Split(v, ",")...)
		return nil
	})
	verify := fs.String("verify", "", "also prove where dispatch <id> ran: workspace, model, fallback, proxy requests")
	check := fs.Bool("check", false, "run gofmt, build, vet, lint, complexity, test, quality and docs on the touched packages")
	batch := fs.Bool("batch", false, "with --check: validate this session's queued handbacks in one batch")
	var gates []string
	fs.Func("gates", "gates: lint, docs, yaml, shell, test or names; comma list", func(v string) error {
		gates = append(gates, v)
		return nil
	})
	all := fs.Bool("all", false, "with --check: the whole tree, not just the changes")
	format := helptext.FormatFlag(fs)
	if code, done := parseWithPaths(fs, args, &sel); done {
		return code
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents diff: %v", err)
	}
	return inspect.Diff(inspect.DiffOptions{Dir: cwd, Base: *base, Paths: sel, Verify: *verify, Check: *check, Batch: *batch, Gates: gates, All: *all, JSON: *format == helptext.JSON,
		Out: os.Stdout, Err: os.Stderr})
}

// cmdAgentsFix is `agents fix`: the make format fixers, scoped to the --path files.
func cmdAgentsFix(args []string) int {
	fs := agentsFlags("fix")
	var sel []string
	fs.Func("path", "files or directories to fix: a,b or repeat; trailing args add more (required)", func(v string) error {
		sel = append(sel, strings.Split(v, ",")...)
		return nil
	})
	dry := fs.Bool("dry-run", false, "list the files each fixer would rewrite; change nothing")
	if code, done := parseWithPaths(fs, args, &sel); done {
		return code
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents fix: %v", err)
	}
	return inspect.Fix(inspect.FixOptions{Dir: cwd, Paths: sel, DryRun: *dry, Out: os.Stdout, Err: os.Stderr})
}

func cmdAgentsFind(args []string) int {
	fs := agentsFlags("find")
	o := inspect.FindOptions{Dir: ".", Out: os.Stdout, Err: os.Stderr}
	fs.BoolVar(&o.Name, "name", false, "match file paths instead of content")
	fs.BoolVar(&o.Func, "func", false, "match Go function or method declarations")
	fs.BoolVar(&o.Def, "def", false, "match Go top-level func, type, const or var declarations")
	fs.BoolVar(&o.Callers, "callers", false, "name-based Go calls to exactly pattern; no type resolution")
	fs.BoolVar(&o.IgnoreCase, "i", false, "ignore case")
	fs.BoolVar(&o.Word, "w", false, "match whole words")
	fs.BoolVar(&o.Files, "l", false, "print matching file names only")
	fs.IntVar(&o.Max, "max", inspect.FindDefaultMax, "result-line budget; more is a lower bound when cut")
	fs.Func("path", "limit to a repo path prefix or glob; repeat per path (as agents diff)", func(v string) error {
		o.Paths = append(o.Paths, v)
		return nil
	})
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	o.Pattern, o.JSON = pos[0], *format == helptext.JSON
	return inspect.Find(o)
}

// cmdAgentsWaves is `agents ask --waves <plan>`: the wave runner.
func cmdAgentsWaves(args []string) int {
	fs := helptext.NewFlagSet("agents ask --waves", helptext.Page{Usage: "claude-code agents ask --waves <plan> [flags]",
		About: "Run a plan’s waves with observed verification and no commits; the shared route flags reach every leaf.",
		Hint:  "Exit 0 passed, 1 failed wave, 2 usage or invalid plan,\n3 outside owned files or unattributed, 4 interrupted."})
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
	waveOpts.Timeout, waveOpts.JSON = *timeout, *format == helptext.JSON
	return dispatchWaves(pos, route, waveOpts, waveJob{*background, *job, args})
}

// cmdAgentsWait is `agents wait <id>...`: one row per settled dispatch; exit 1 on error or lost, 4 on timeout.
func cmdAgentsWait(args []string) int {
	fs := agentsFlags("wait")
	timeout := fs.Duration("timeout", waitDefaultTimeout, "give up on unfinished dispatches after this long (under the 10m Bash cap)")
	progress := fs.Bool("progress", false, "print a stderr line each time a job's activity changes")
	format := helptext.FormatFlag(fs)
	ids, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(ids) == 0 {
		fs.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rows, err := waitRows(ctx, ids, *timeout, *progress)
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
	printResumeHints(rows)
	return agent.WaitExit(rows)
}

// waitDefaultTimeout keeps one wait call under the 600000 ms Bash cap; repeat it to cover a 15m dispatch.
const waitDefaultTimeout = 570 * time.Second

// printResumeHints prints each unfinished or failed wait row's resume line on stderr.
func printResumeHints(rows []agent.WaitResult) {
	for _, r := range rows {
		if r.State == agent.WaitDone {
			continue
		}
		if r.State == agent.WaitTimeout {
			fmt.Fprintf(os.Stderr, "%s: still running, re-run `claude-code agents result -f %s`\n", r.ID, r.ID)
			continue
		}
		if rec, err := agent.LoadRecord(r.ID); err == nil {
			if hint := agent.ResumeHint(rec); hint != "" {
				fmt.Fprintln(os.Stderr, hint)
			}
		}
	}
}

// Implementation briefs require both an ownership boundary and a verification criterion.
func cmdAgentsBrief(args []string) int {
	fs := agentsFlags("brief")
	pathMode := fs.Bool("path", false, "reserve a unique empty brief file; optional agent argument")
	taskFile := fs.String("task", "", "the authored task text file; - reads stdin")
	var input agent.BriefInput
	fs.Func("own", "a file or directory the leaf may edit; repeat per path", func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("owned path cannot be blank")
		}
		input.Owned = append(input.Owned, v)
		return nil
	})
	fs.StringVar(&input.Report, "report", "", "where the leaf writes its full report")
	fs.StringVar(&input.Verify, "verify", "", "how to verify completion")
	fs.StringVar(&input.Agent, "agent", "", "agent named in the brief header")
	fs.StringVar(&input.PlanTask, "plan-task", "", "plan task id the brief's Task: line carries")
	fs.StringVar(&input.Tier, "tier", "", "task tier in the brief header: "+strings.Join(agent.TierNames(), "|"))
	fs.Func("codemode", "true|false: the target run's code mode (default: this shell's CODEMODE)", func(v string) error {
		if v != "true" && v != "false" {
			return fmt.Errorf("want true or false, got %q", v)
		}
		on := v == "true"
		input.CodeMode = &on
		return nil
	})
	shape := briefShapeFlags(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if shape.id != "" {
		return renderShapeBrief(fs, shape, pos)
	}
	if *pathMode {
		return printBriefPath(fs, pos)
	}
	if *taskFile == "" || len(pos) > 0 || len(input.Owned) == 0 || strings.TrimSpace(input.Verify) == "" {
		fs.Usage()
		return 2
	}
	return renderAgentsBrief(*taskFile, input)
}

// shapeFlags is the typed input of `agents brief --shape`.
type shapeFlags struct {
	id, from      string
	write, schema *bool
	in            agent.SkillRequest
}

func briefShapeFlags(fs *flag.FlagSet) *shapeFlags {
	sf := &shapeFlags{write: fs.Bool("write", false, "with --shape: create the files under the config dir, never overwriting"),
		schema: fs.Bool("synthesis-schema", false, "with --shape skill: print an example synthesis record to fill")}
	fs.StringVar(&sf.from, "from", "", "with --shape skill: a synthesis record (JSON file, - for stdin) to fill the body")
	fs.StringVar(&sf.id, "shape", "", "render a new prompt file of this shape ("+strings.Join(agent.ShapeIDs(), "|")+")")
	fs.StringVar(&sf.in.Name, "name", "", "with --shape: the file stem (the skill's directory name)")
	fs.StringVar(&sf.in.Description, "description", "", "with --shape: what it does, one line")
	fs.StringVar(&sf.in.NotFor, "not-for", "", "with --shape: what belongs elsewhere")
	fs.Func("trigger", "with --shape: a phrase for the Use for clause; repeat", func(v string) error {
		sf.in.Triggers = append(sf.in.Triggers, strings.TrimSpace(v))
		return nil
	})
	fs.Func("non-trigger", "with --shape: a near-miss prompt; repeat", func(v string) error {
		sf.in.NonTriggers = append(sf.in.NonTriggers, strings.TrimSpace(v))
		return nil
	})
	fs.Func("task-case", "with --shape: a task and its correct result; repeat", func(v string) error {
		sf.in.TaskCases = append(sf.in.TaskCases, strings.TrimSpace(v))
		return nil
	})
	fs.Func("reference", "with --shape: a references/<stem>.md stub; repeat", func(v string) error {
		sf.in.Refs = append(sf.in.Refs, strings.TrimSpace(v))
		return nil
	})
	return sf
}

func renderShapeBrief(fs *flag.FlagSet, sf *shapeFlags, pos []string) int {
	if !slices.Contains(agent.ShapeIDs(), sf.id) {
		return helptext.Fail("agents brief: unknown --shape %q; known: %s", sf.id, strings.Join(agent.ShapeIDs(), ", "))
	}
	if *sf.schema {
		return printOut(agent.SynthesisExample())
	}
	if len(pos) > 0 || sf.in.Name == "" {
		fs.Usage()
		return 2
	}
	if err := sf.in.ReadSynthesis(sf.from); err != nil {
		return helptext.Fail("agents brief: --from %s: %v", sf.from, err)
	}
	out, err := agent.ScaffoldSkill(paths.ConfigDir(), sf.in, *sf.write)
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	return printOut(out)
}

func printOut(out string) int {
	if _, err := fmt.Print(out); err != nil {
		return helptext.Fail("agents brief: write output: %v", err)
	}
	return 0
}

func renderAgentsBrief(taskFile string, input agent.BriefInput) int {
	task, err := agent.ReadBrief(taskFile)
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	if strings.TrimSpace(task) == "" {
		return helptext.Fail("agents brief: --task %q is empty", taskFile)
	}
	input.Task = task
	out, err := agent.RenderBrief(input)
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	if _, err := fmt.Print(out); err != nil {
		return helptext.Fail("agents brief: write output: %v", err)
	}
	return 0
}

func printBriefPath(fs *flag.FlagSet, pos []string) int {
	if fs.NFlag() != 1 || len(pos) > 1 {
		fs.Usage()
		return 2
	}
	path, err := agent.NewBriefPath(strings.Join(pos, ""))
	if err != nil {
		return helptext.Fail("agents brief: %v", err)
	}
	if _, err := fmt.Println(path); err != nil {
		return helptext.Fail("agents brief: write path: %v", err)
	}
	return 0
}

// cmdAgentsMsg is `agents msg [@name|#name] [text|-]`: with text or a file it sends, without it reads what is new.
func cmdAgentsMsg(args []string) int {
	fs := agentsFlags("msg")
	from := fs.String("from", "", "who you are; default the calling Claude session's name")
	format := fs.String("format", helptext.Text, "how reads print: text or json")
	var files []string
	helptext.ListVar(fs, &files, "file", "PATH", "attach a file (up to 8 MiB); repeatable", false)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	return runMsg(pos, *from, *format, files)
}

// cmdAgentsMcp is `agents mcp --from <agent>`: the harness tools as a stdio MCP server.
func cmdAgentsMcp(args []string) int {
	fs := agentsFlags("mcp")
	from := fs.String("from", "", "who you are; default the calling Claude session's name")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 0 {
		return helptext.FailUsage("agents mcp: takes no arguments, got %q", pos[0])
	}
	return ask.ServeHarness(*from)
}

// runMsg sends when there is text or a file, else reads; pos[0] is the @name or #name target when present.
func runMsg(pos []string, from, format string, files []string) int {
	chats := pos[:min(len(pos), 1)]
	if len(chats) == 1 && !strings.HasPrefix(chats[0], "@") && !strings.HasPrefix(chats[0], "#") {
		return helptext.FailUsage("agents msg: the target is @name (an agent) or '#name' (a room), got %q", pos[0])
	}
	switch {
	case len(pos) >= 2 && pos[0] == ask.ParentChat: // literal: "-" and "@file" are note text here
		return ask.NoteParent(strings.Join(pos[1:], " "))
	case len(pos) >= 2 || (len(chats) == 1 && len(files) > 0):
		return ask.SendMsg(chats[0], strings.Join(pos[1:], " "), from, files)
	case len(files) > 0:
		return helptext.FailUsage("agents msg: --file needs a target, as in @gemini")
	}
	return ask.ReadMsgs(chats, from, format)
}

// cmdAgentsSend is `agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := agentsFlags("send")
	format := fs.String("format", helptext.Text, "stream text, print the JSON envelope, or stream-json NDJSON events")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up waiting for a running dispatch after this long")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains(agent.Formats, *format) {
		fs.Usage()
		return 2
	}
	return ask.Send(pos[0], strings.Join(pos[1:], " "), *format, *timeout)
}

// agentAbout expands each agent's one-line menu summary for its help page.
var agentAbout = map[string]string{
	"copilot": "GitHub's Copilot CLI on its own login, or the selected Databricks profile. Bare " +
		"launches it with args forwarded; the commands below run it headless, in the background or as a reviewer.",
	"codex": "OpenAI's Codex CLI on your ChatGPT login, the second-opinion reviewer. Bare " +
		"launches it with args forwarded; the commands below run it headless.",
	"gemini": "Gemini through agy (the Antigravity CLI) on your Google sign-in. Bare launches it " +
		"with args forwarded; the commands below run it headless.",
	"opencode": "The OpenCode CLI, a general code agent with the widest model pool (catalog), on its " +
		"own login or the selected Databricks profile. Bare launches it with args forwarded; the " +
		"commands below run it headless.",
	"pi": "The Pi coding agent on the selected Databricks profile through the loopback proxy, read-only. " +
		"The commands below run it headless.",
}

// agentPage is the help page for one agent verb: the shared command roster plus the CLI's extra rows.
func agentPage(name string) helptext.Page {
	rows := [][2]string{
		{"review", "diff review; --adversarial"},
		{"transfer", "hand this Claude session to " + name + "; --source"},
		{"catalog", "model catalog; on Databricks: list|check|sync"},
		{"profile", "show or set " + name + "'s profile; --clear = own login; --off"},
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
	if name == "codex" {
		rows = append(rows, [2]string{"cloud", "Codex Cloud tasks: exec|status|list|diff|apply"})
	}
	binary := name
	if s.External() {
		binary = s.CLI.Binary
	}
	usage := "claude-code " + name + " [<command>|--] [launch flags] [args…]"
	hint := ""
	if s.CLI.ProviderVerb {
		usage += "\nclaude-code " + name + " <provider> [-p NAME] [rest…]"
		hint = "\nclaude-code agents ask --agent " + name + " --profile dev \"Explain this code\""
	}
	return helptext.Page{
		Usage:    usage,
		About:    agentAbout[name],
		Commands: rows,
		Flags:    launchFlags(name),
		Hint: "claude-code " + name + " <command> -h lists that command's flags.\n" +
			"claude-code " + name + " " + helptext.DocsFlag + " prints " + binary + "'s own help." + hint,
	}
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
	"catalog": {"catalog [list|check|sync] [flags]", "Databricks serving endpoints %s can use, or its own model catalog."},
	"review":  {"review [flags] [focus…]", "Review a diff with %s; flag-like focus words go after --."},
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
	return helptext.Page{Usage: "claude-code " + name + " " + sp[0], About: about}
}

// newAgentFlags makes a ContinueOnError FlagSet named "<agent> <verb>" whose -h renders the sub-verb's page.
func newAgentFlags(name, verb string) *flag.FlagSet {
	return helptext.NewFlagSet(name+" "+verb, agentSubPage(name, verb))
}

// retiredAgentVerbs are refused, not forwarded to the agent CLI; their jobs moved to agents verbs and ask --session.
var retiredAgentVerbs = []string{"ask", "explore", "resume", "status", "tail", "result", "cancel", "on", "off", "setup"}

// retiredVerb is the retired verb args open with past the harness launch flags; else "" (a native flag stops it).
func retiredVerb(name string, args []string) string {
	fs := launchFlags(name)
	for i := 0; i < len(args); i++ {
		word, _, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		f := fs.Lookup(word)
		switch {
		case !strings.HasPrefix(args[i], "-"):
			if slices.Contains(retiredAgentVerbs, args[i]) {
				return args[i]
			}
			return ""
		case args[i] == "--" || f == nil:
			return ""
		case !hasVal && !isBoolValue(f.Value):
			i++
		}
	}
	return ""
}

func isBoolValue(v flag.Value) bool {
	b, ok := v.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func agentProviderArgs(name string, args []string) ([]string, error) {
	if !agent.SchemaFor(name).CLI.ProviderVerb || len(args) == 0 || !slices.Contains(providers.ProviderKeys(), args[0]) {
		return args, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	provider, rest := args[0], args[1:]
	rows := reg.ProfilesOf(provider)
	if !slices.ContainsFunc(rows, func(n string) bool { return agent.CanServe(name, reg.Row(n)) }) {
		return args, nil
	}
	option, profile, equals := strings.Cut(firstPositional(rest), "=")
	if !slices.Contains([]string{"-p", "--profile"}, option) {
		signedIn := slices.DeleteFunc(slices.Clone(rows), func(n string) bool { return !providers.CredentialPresent(reg.Row(n)) })
		return nil, fmt.Errorf("%s %s: name a profile: -p %s", name, provider, strings.Join(signedIn, "|"))
	}
	if !equals && len(rest) > 1 {
		profile, rest = rest[1], rest[1:]
	}
	if profile == "" || strings.HasPrefix(profile, "-") {
		return nil, fmt.Errorf("%s %s: %s requires a profile name", name, provider, option)
	}
	p, err := reg.Get(profile)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, provider, err)
	}
	if p.Provider != provider {
		return nil, fmt.Errorf("%s %s: profile %q does not belong to %s", name, provider, profile, provider)
	}
	return append([]string{"--profile", profile}, rest[1:]...), nil
}

var agentLaunch = ask.Launch

// agentOwnVerbs are one agent's own subcommands (not shared pages), keyed by agent and verb.
var agentOwnVerbs = map[[2]string]func([]string) int{{"codex", "cloud"}: codexCloud, {"gemini", "vscode"}: geminiVSCode}

// cmdAgentVerb is `<agent>`: bare or `--` launches, a sub-verb runs it, else launch.
func cmdAgentVerb(name string, args []string) int {
	args, err := agentProviderArgs(name, args)
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	retired := retiredVerb(name, args)
	switch {
	case len(args) == 0 || args[0] == "--":
		return agentLaunch(name, args)
	case retired != "":
		return unknownSub(name, retired, agentPage(name))
	case agentOwnVerbs[[2]string{name, args[0]}] != nil:
		return agentOwnVerbs[[2]string{name, args[0]}](args[1:])
	case agentSubPages[args[0]] != [2]string{}:
		return agentSub(name, args)
	case len(args) == 1 && isHelpVerb(args[0]):
		agentPage(name).Fprint(os.Stderr)
		return 0
	case len(args) == 1 && args[0] == helptext.DocsFlag:
		s, _ := agent.LookupSchema(name)
		return helptext.WrappedDocs(s.CLI.Binary)
	}
	return agentLaunch(name, args)
}

// agentSub dispatches an agent's subverbs (catalog, transfer, profile, review).
func agentSub(name string, args []string) int {
	handlers := map[string]func(string, []string) int{
		"catalog":  agentCatalog,
		"transfer": agentTransfer, "profile": agentProfile, "review": agentReview,
	}
	if run := handlers[args[0]]; run != nil {
		return run(name, args[1:])
	}
	return helptext.Fail("%s: unknown subcommand %q", name, args[0])
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
	fs.StringVar(&o.Overlay, "overlay", "", `{"cloud":true} runs claude ultrareview in Anthropic's cloud`)
	format := helptext.FormatFlag(fs)
	repeated := guardScopeFlags(fs)
	pos, err := helptext.ParseArgs(fs, args) // flags may follow `--pr 12`
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *repeated != "" {
		return helptext.FailUsage("%s review: --%s given more than once; put flag-like focus text after --", name, *repeated)
	}
	o.Format = *format
	o.Focus = strings.Join(pos, " ")
	return ask.Review(name, args, o)
}

// onceFlag records a scope flag set twice: the flag package keeps only the last value.
type onceFlag struct {
	flag.Value
	name     string
	repeated *string
	seen     bool
}

func (f *onceFlag) Set(s string) error {
	if f.seen {
		*f.repeated = f.name
	}
	f.seen = true
	return f.Value.Set(s)
}

func (f *onceFlag) IsBoolFlag() bool { return isBoolValue(f.Value) }

// guardScopeFlags wraps the review scope flags; the result names the first one given twice, else "".
func guardScopeFlags(fs *flag.FlagSet) *string {
	repeated := new(string)
	for _, n := range []string{"uncommitted", "base", "commit", "pr"} {
		f := fs.Lookup(n)
		f.Value = &onceFlag{Value: f.Value, name: n, repeated: repeated}
	}
	return repeated
}

var transferAgent = ask.Transfer

// agentTransfer is `<v> transfer [--source f] [--wait] [note…]`.
func agentTransfer(name string, args []string) int {
	fs := newAgentFlags(name, "transfer")
	source := fs.String("source", "", "transcript .jsonl (default: newest session for this repo)")
	wait := fs.Bool("wait", false, "block for the answer instead of detaching")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	return transferAgent(name, *source, strings.Join(pos, " "), *wait)
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

// geminiVSCodePage is `gemini vscode -h`: the launch flags, which pick the route as a bare launch does.
func geminiVSCodePage() helptext.Page {
	p := helptext.Page{
		Usage: "claude-code gemini vscode [--profile P] [--overlay JSON] [--model M] [code args…]",
		About: "Open a VS Code window whose Antigravity extension uses its Google login, or with --profile or " +
			"--overlay a Databricks profile's gateway. Other windows keep their route. Other args go to VS Code " +
			"(a folder). macOS. Exit 0, 1 failed.",
		Flags: launchFlags("gemini"),
	}
	return p
}

// geminiVSCode is `gemini vscode [launch flags] [code args…]`; --here is the detached helper's form.
func geminiVSCode(args []string) int {
	if len(args) == 1 && isHelpVerb(args[0]) {
		geminiVSCodePage().Fprint(os.Stderr)
		return 0
	}
	rest := slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--here" })
	return ask.VSCode("gemini", len(rest) < len(args), rest)
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

// cmdAgentsCodemode is `agents codemode on|off|status [--session ID]`.
func cmdAgentsCodemode(args []string) int {
	fs := agentsFlags("codemode")
	session := fs.String("session", os.Getenv(paths.SessionIDEnv), "the Claude session id (default: this session)")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || !slices.Contains([]string{"on", "off", "status"}, pos[0]) {
		fs.Usage()
		return 2
	}
	if *session == "" {
		return helptext.FailUsage("agents codemode: no session: pass --session or run inside a Claude session")
	}
	if pos[0] != "status" {
		if err := rtkwrap.SetSessionFlag(*session, pos[0] == "on"); err != nil {
			return helptext.FailUsage("agents codemode: %v", err)
		}
	}
	line, err := rtkwrap.CodeModeStatus(*session)
	if err != nil {
		return helptext.FailUsage("agents codemode: %v", err)
	}
	fmt.Println(line)
	return 0
}
