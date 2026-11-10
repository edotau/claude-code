package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/hooks"
	hookagents "github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// engineNames lists the `ask --agent` agent names; agent.Engines() only builds the roster, it never dials an agent.
func engineNames() []string {
	var out []string
	for _, r := range agent.Engines() {
		out = append(out, r.Name())
	}
	return out
}

func askSpecFlags() []helptext.Flag {
	flag, boolFlag, valuedFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag
	return []helptext.Flag{
		valuedFlag("agent", engineNames), flag("agents"), valuedFlag("profile", providers.ProfileNames), flag("model"),
		flag("effort"), flag("workdir"), flag("session"), boolFlag("tools"), boolFlag("read-only"), boolFlag("no-tools"), boolFlag("native"), flag("allow"), flag("ask"), flag("deny"),
		valuedFlag("mode", func() []string { return []string{"default", "auto", "plan"} }),
		flag("timeout"), flag("format"), flag("rubric"), flag("retry"), boolFlag("log"),
		valuedFlag("fallback", func() []string { return router.FallbackModes }),
		flag("attach"), flag("keep-env"), flag("overlay"), boolFlag("autonomous"), boolFlag("fleet"), boolFlag("enable-memory"),
		boolFlag("background"), flag("job"), flag("batch"), flag("jobs"), boolFlag("fail-fast"),
		boolFlag("no-persist"), boolFlag("safe-mode"), flag("debug-file"), valuedFlag("as", func() []string { return []string{"user", "sp"} }),
		flag("append-system-prompt"), flag("system-prompt"), flag("output-schema"), flag("env"), flag("provider"), flag("host"), flag("remote-bin"),
		boolFlag("live"), boolFlag("direct"), valuedFlag("route", func() []string { return router.RouteModes }), flag("retry-agent"),
		boolFlag("diff"), flag("dirs"), flag("files"), boolFlag("index"), flag("max-files"), flag("max-file-bytes"),
		boolFlag("follow"), flag("follow-idle"), flag("overlay-debug"), boolFlag("stdin-framed"),
	}
}

// agentsSpec is the completion node for the agents verb.
func agentsSpec() *helptext.Node {
	flag, boolFlag, valuedFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"sync": {Flags: []helptext.Flag{valuedFlag("agent", project.Names), boolFlag("dry-run"), boolFlag("check"), boolFlag("no-prune")}},
		"tree": {Flags: []helptext.Flag{flag("days"), flag("session"), boolFlag("background"), boolFlag("bg"),
			boolFlag("watch"), flag("interval"), formatFlag()}},
		"reap":  {Flags: []helptext.Flag{boolFlag("dry-run")}},
		"label": {Flags: []helptext.Flag{flag("model"), flag("brief"), flag("session-model"), formatFlag()}},
		"list":  {Flags: []helptext.Flag{flag("host"), flag("remote-bin"), formatFlag()}},
		"ask":   {Flags: askSpecFlags()},
		"dispatch": {Args: []func() []string{func() []string { return []string{"explore", "fleet", "compare"} }}, Flags: []helptext.Flag{
			flag("part"), boolFlag("no-web"), flag("models"), flag("jobs"), flag("timeout"), formatFlag(), boolFlag("background"),
			flag("output-schema")}},
		"send":   {Flags: []helptext.Flag{flag("format")}},
		"status": {Flags: []helptext.Flag{formatFlag()}},
		"result": {Flags: []helptext.Flag{formatFlag()}},
		"tail":   {Flags: []helptext.Flag{formatFlag()}},
		"cancel": {Flags: []helptext.Flag{formatFlag()}},
	}}
}

// anthropicSpec is the completion node for the anthropic verb.
func anthropicSpec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{"science": {}},
		Flags: []helptext.Flag{boolFlag("oauth"), boolFlag("x-api-key"), flag("model"), flag("profile"),
			boolFlag("print-env"), flag("keep-env"), flag("overlay")}}
}

// hookSpec is the completion node for the hook verb.
func hookSpec() *helptext.Node { return &helptext.Node{Args: []func() []string{settings.HookEvents}} }

// agentSpec is the one completion subtree for every agent verb (codex, copilot, opencode, gemini).
func agentSpec() *helptext.Node {
	flag, boolFlag, formatFlag := helptext.Plain, helptext.BoolFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ask":    {Flags: askSpecFlags()},
		"resume": {},
		"review": {Flags: []helptext.Flag{boolFlag("uncommitted"), flag("base"), flag("commit"), boolFlag("pr"),
			boolFlag("adversarial"), boolFlag("background"), flag("rubric")}},
		"explore":  {Flags: []helptext.Flag{boolFlag("no-web"), boolFlag("background"), formatFlag()}},
		"transfer": {Flags: []helptext.Flag{flag("source"), boolFlag("wait")}},
		"status":   {Flags: []helptext.Flag{boolFlag("all"), formatFlag()}},
		"tail":     {Flags: []helptext.Flag{boolFlag("f")}},
		"result":   {Flags: []helptext.Flag{formatFlag()}},
		"cancel":   {},
		"profile":  {Args: []func() []string{providers.ProfileNames}, Flags: []helptext.Flag{boolFlag("clear")}},
		"on":       {},
		"off":      {},
		"setup":    {Flags: []helptext.Flag{formatFlag()}},
		"catalog":  {},
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
		{"dispatch", "run a task by role: explore, fleet, compare"},
		{"sync", "project agents/ + CLAUDE.md into every agent home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"reap", "mark dead running dispatches as error; drop stale markers"},
		{"label", "print a leaf's dispatch description prefix"},
		{"status", "show a dispatch's state (any agent): status <id>"},
		{"result", "print a finished dispatch's answer or envelope: result <id>"},
		{"tail", "follow a dispatch's log or events: tail <id> [-f]"},
		{"cancel", "interrupt a running dispatch: cancel <id>"},
		{"send", "send one turn to a live claude or gemini session: send <id> <task>"},
	},
	Hint: "claude-code agents <command> -h lists that command's flags.",
}

// agentsSubPages are the `agents <verb> -h` pages: usage tail, blurb and hint.
var agentsSubPages = map[string][3]string{
	"list": {"list [flags]", "List each agent, whether it is available and which request fields (model, profile, effort, …) it honors."},
	"reap": {"reap [flags]", "Mark dispatch records still \"running\" whose process is gone as error, and drop stale " +
		"agent-inflight markers."},
	"sync": {"sync [flags]", "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills."},
	"tree": {"tree [flags]", "Live tree of running Claude sessions, their subagents and agent dispatches, in every repo. " +
		"--background adds idle and finished work."},
	"label": {"label <type> [flags]", "Print the \"<model>: \" prefix the dispatch-effort hook requires on a dispatch " +
		"description, for a leaf of <type> with the given model and brief."},
	"send": {"send <id> <task | -> [flags]", "Send one user turn to a live claude or gemini session (ask --agent <a> " +
		"--live) and stream that turn's events; exits on its result. One turn at a time per session."},
	"dispatch": {"dispatch <role> [flags] <task>", "Run a task by role: explore (gemini, one leg per --part), fleet " +
		"(copilot fleet mode) or compare (opencode, one leg per model). Legs run concurrently; the report lists each leg's answer.",
		"Roles come from the vault overlay dispatch-roles, else the built-in table."},
	"status": {"status <id> [flags]", "Show one dispatch's state, whichever agent ran it (claude included)."},
	"result": {"result <id> [flags]", "Print a finished dispatch's output; --format json prints its envelope."},
	"tail":   {"tail <id> [flags]", "Print a dispatch's log; --format json streams its events.jsonl instead."},
	"cancel": {"cancel <id> [flags]", "Interrupt a running background dispatch, whichever agent runs it."},
}

// agentsFlags makes the `agents <verb>` FlagSet whose -h renders its page.
func agentsFlags(verb string) *flag.FlagSet {
	sp := agentsSubPages[verb]
	return helptext.NewFlagSet("agents "+verb, helptext.Page{Usage: "claude-code agents " + sp[0], About: sp[1], Hint: sp[2]})
}

// cmdAgents is `claude-code agents`.
func cmdAgents(args []string) int {
	if len(args) == 0 || slices.Contains([]string{"-h", "--help", "help"}, args[0]) {
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	if run := agentsVerbs[args[0]]; run != nil {
		return run(args[1:])
	}
	helptext.Fail("agents: unknown subcommand %q", args[0])
	agentsPage.Fprint(os.Stderr)
	return 1
}

// agentsVerbs routes `agents <verb>` to its handler; the job verbs share cmdAgentsJob.
var agentsVerbs = map[string]func([]string) int{
	"list": cmdAgentsList, "ask": ask.Cmd, "dispatch": cmdAgentsDispatch, "sync": cmdAgentsSync,
	"tree": cmdAgentsTree, "reap": cmdAgentsReap, "send": cmdAgentsSend, "label": cmdAgentsLabel,
	"status": func(a []string) int { return cmdAgentsJob("status", a) },
	"result": func(a []string) int { return cmdAgentsJob("result", a) },
	"tail":   func(a []string) int { return cmdAgentsJob("tail", a) },
	"cancel": func(a []string) int { return cmdAgentsJob("cancel", a) },
}

// cmdAgentsList is `agents list`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := agentsFlags("list")
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents list --format)")
	remoteBin := fs.String("remote-bin", "", "remote claude-code path for --host (default $"+remote.BinEnv+", then claude-code)")
	format := fs.String("format", "text", "text, or json: per agent {name, binary, available, reason, caps, events}")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || !slices.Contains([]string{helptext.Text, helptext.JSON}, *format) {
		fs.Usage()
		return 2
	}
	if *host != "" {
		if err := client.ListRemote(*host, remote.Bin(*remoteBin), *format, os.Stdout); err != nil {
			return helptext.Fail("%v", err)
		}
		return 0
	}
	return client.PrintRoster(*format)
}

// cmdAgentsReap marks dead-but-"running" dispatch records as error and drops stale agent-inflight markers.
func cmdAgentsReap(args []string) int {
	fs := agentsFlags("reap")
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
	fs := agentsFlags("sync")
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

// cmdAgentsTree is `agents tree`; --watch clears and redraws every --interval until a render fails.
func cmdAgentsTree(args []string) int {
	fs := agentsFlags("tree")
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
	return tree.Show(paths.ConfigDir(), opts, tree.View{JSON: *format == helptext.JSON, All: background || *session != "",
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
	brief := fs.String("brief", "", "the brief file the leaf gets; - reads stdin")
	sessionModel := fs.String("session-model", "", "the session's model id (default: from this session's transcript)")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || !slices.Contains([]string{helptext.Text, helptext.JSON}, *format) {
		fs.Usage()
		return 2
	}
	task, err := hookagents.ReadBrief(*brief)
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
	if row.Label != "" { // an empty label is no prefix, as the hook writes none
		if _, err := fmt.Println(row.Label + ": "); err != nil {
			return helptext.Fail("agents label: %v", err)
		}
	}
	return 0
}

// partFlag collects repeatable --part entries.
type partFlag []string

func (p *partFlag) String() string { return strings.Join(*p, ",") }

// Placeholder names the FLAGS placeholder for partFlag (helptext.Placeholder).
func (p *partFlag) Placeholder() string { return "QUESTION" }

// Set appends one sub-question.
func (p *partFlag) Set(s string) error { *p = append(*p, s); return nil }

// cmdAgentsDispatch is `agents dispatch <role> <task>`.
func cmdAgentsDispatch(args []string) int {
	fs := agentsFlags("dispatch")
	var parts partFlag
	fs.Var(&parts, "part", "explore: one sub-question per leg (repeatable, max 12)")
	noWeb := fs.Bool("no-web", false, "explore: code scan only, skip web research")
	models := fs.String("models", "", "compare: model ids, comma-separated (default: the role's set)")
	jobs := fs.Int("jobs", roles.MaxParts, "legs running at once (max 12)")
	timeout := fs.Duration("timeout", 0, "per-leg time limit (0 = the agent's default)")
	format := helptext.FormatFlag(fs)
	background := fs.Bool("background", false, "detach each leg and print its id; no merged report")
	outputSchema := fs.String("output-schema", "", "JSON Schema (file or inline) each answer must satisfy")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("dispatch", err)
	}
	if len(pos) < 2 {
		fs.Usage()
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents dispatch: %v", err)
	}
	q := roles.Request{Task: strings.Join(pos[1:], " "), WorkDir: cwd, Parts: parts, Models: roles.SplitModels(*models),
		Web: !*noWeb, Timeout: *timeout}
	return ask.Dispatch(q, ask.DispatchOptions{Role: pos[0], Schema: *outputSchema, Format: *format, Jobs: *jobs, Background: *background})
}

// cmdAgentsJob is `agents status|result|tail|cancel <id>`: the agent comes from the dispatch record.
func cmdAgentsJob(verb string, args []string) int {
	fs := agentsFlags(verb)
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
	switch verb {
	case "status":
		return ask.Status("agents status", rec.Agent, rec.ID, true, *format, "")
	case "result":
		return ask.PrintResult(rec, *format)
	case "tail":
		return ask.TailJob("agents tail", rec, *format, *follow)
	}
	return ask.CancelJob("agents cancel", rec.Agent, rec.ID, *format)
}

// cmdAgentsSend is `agents send <id> <task>`.
func cmdAgentsSend(args []string) int {
	fs := agentsFlags("send")
	format := fs.String("format", helptext.Text, "stream text, print the JSON envelope, or stream-json NDJSON events")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains([]string{helptext.Text, helptext.JSON, client.FormatStreamJSON}, *format) {
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
		{"resume", "continue a session"},
		{"review", "diff review; --adversarial"},
		{"explore", "scan code + web research → brief"},
		{"transfer", "hand this Claude session to " + name + "; --source"},
		{"status", "background jobs in this repo; --all"},
		{"tail", "a job's output; -f follows live"},
		{"result", "a finished job's answer + session"},
		{"cancel", "interrupt a running job"},
		{"setup", "binary, login, route, projection; --format json"},
		{"catalog", "the agent's own model catalog"},
		{"profile", "show or set " + name + "'s harness profile pin"},
		{"on", "route to " + name + " again"},
		{"off", "stop routing to " + name},
	}
	s, _ := agent.LookupSchema(name)
	if s.CLI.CatalogUsage != "" {
		rows[slices.IndexFunc(rows, func(r [2]string) bool { return r[0] == "catalog" })][1] = s.CLI.CatalogUsage
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

// launchFlags lists the harness flags a bare launch consumes (launchcli.ParseArgs) for the help page.
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
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	if name == "codex" {
		fs.String("f", "", "config.toml whose keys ride as -c overrides")
	}
	return fs
}

// agentSubPages maps each sub-verb to its synopsis tail and blurb; %s is the agent name.
var agentSubPages = map[string][2]string{
	"resume": {"resume <id> [ask flags] <task>",
		"Continue session <id> on %s (= ask --agent %[1]s --session <id>); ask's flags apply, see claude-code agents ask -h."},
	"status":   {"status [id] [flags]", "Background %s jobs in this repo, newest first; an id narrows to one job."},
	"tail":     {"tail [id] [flags]", "A job's output (default: the newest). Exits 1 when the job failed or was lost."},
	"result":   {"result [id] [flags]", "A finished job's answer (default: the newest), its session and resume command."},
	"cancel":   {"cancel", "Interrupt a running %s job (default: the single running job in this repo)."},
	"setup":    {"setup [flags]", "Check %s's binary, login, route and projection; exits 1 when one fails."},
	"catalog":  {"catalog [--profile p]", "The %s CLI's own model catalog, or a note that it has none."},
	"review":   {"review [flags] [focus…]", "Review a diff with %s; trailing words name the focus."},
	"explore":  {"explore [flags] <question>", "Scan the code and research the web with %s, then return a brief."},
	"profile":  {"profile [<profile>|--clear]", "Show %s's profile pin, or set %[1]s to <profile> (--clear unpins)."},
	"on":       {"on", "Route to %s again (clears its off pin)."},
	"off":      {"off", "Stop routing to %s (claude cannot be turned off)."},
	"transfer": {"transfer [flags] [note…]", "Hand this Claude session's transcript to %s, detached unless --wait."},
}

// agentSubPage is the help page for `<agent> <verb>`.
func agentSubPage(name, verb string) helptext.Page {
	sp := agentSubPages[verb]
	about := sp[1]
	if strings.Contains(about, "%") {
		about = fmt.Sprintf(about, name)
	}
	page := helptext.Page{Usage: "claude-code " + name + " " + sp[0], About: about}
	if s, _ := agent.LookupSchema(name); verb == "catalog" {
		page.Hint = s.CLI.CatalogHint
	}
	return page
}

// newAgentFlags makes a ContinueOnError FlagSet named "<agent> <verb>" whose -h renders the sub-verb's page.
func newAgentFlags(name, verb string) *flag.FlagSet {
	return helptext.NewFlagSet(name+" "+verb, agentSubPage(name, verb))
}

// cmdAgentVerb is `<agent>`: bare or `--` launches, `ask` = `ask --agent <v>`, a sub-verb runs it, else launch.
func cmdAgentVerb(name string, args []string) int {
	switch {
	case len(args) == 0:
		return ask.Launch(name, args)
	case args[0] == "--":
		return ask.Launch(name, args[1:])
	case args[0] == "ask":
		return ask.Cmd(append([]string{"--agent", name}, args[1:]...))
	case agentSubPages[args[0]] != [2]string{}:
		return agentSub(name, args)
	case len(args) == 1 && slices.Contains([]string{"-h", "--help", "help"}, args[0]):
		agentPage(name).Fprint(os.Stderr)
		return 0
	case len(args) == 1 && args[0] == helptext.DocsFlag:
		s, _ := agent.LookupSchema(name)
		return helptext.WrappedDocs(s.CLI.Binary)
	}
	return ask.Launch(name, args)
}

// agentSubHelp prints the sub-verb's page when args open with -h/--help, for sub-verbs without a FlagSet.
func agentSubHelp(name, verb string, args []string) bool {
	if len(args) == 0 || (args[0] != "-h" && args[0] != "--help") {
		return false
	}
	agentSubPage(name, verb).Fprint(os.Stderr)
	return true
}

// agentSub dispatches an agent's background-job and setup subverbs (resume/status/result/cancel/setup/catalog…).
func agentSub(name string, args []string) int {
	handlers := map[string]func(string, []string) int{
		"resume": agentResume, "status": agentStatus, "tail": agentTail,
		"result": agentResult, "cancel": agentCancel, "setup": agentSetup,
		"catalog": agentCatalog, "explore": agentExplore,
		"transfer": agentTransfer, "profile": agentProfile,
		"review": agentReview,
		"on":     func(name string, args []string) int { return agentOnOff(name, true, args) },
		"off":    func(name string, args []string) int { return agentOnOff(name, false, args) },
	}
	if run := handlers[args[0]]; run != nil {
		return run(name, args[1:])
	}
	return helptext.Fail("%s: unknown subcommand %q", name, args[0])
}

// agentResume is `<v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
func agentResume(name string, args []string) int {
	if agentSubHelp(name, "resume", args) {
		return 0
	}
	if len(args) < 1 {
		return helptext.Fail("%s resume: usage: claude-code %s resume <id> <task>", name, name)
	}
	if strings.HasPrefix(args[0], "-") { // flags first: ask's own parser, the session id is its first positional
		return ask.Run(append([]string{"--agent", name}, args...), true)
	}
	return ask.Cmd(append([]string{"--agent", name, "--session", args[0]}, args[1:]...))
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
	fs.BoolVar(&o.Background, "background", false, "detach; claude-code "+name+" status|result to follow up")
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
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
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

// agentStatus is `<v> status [id] [--all] [--format text|json]`.
func agentStatus(name string, args []string) int {
	fs := newAgentFlags(name, "status")
	all := fs.Bool("all", false, "every repo, not just this one")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" status", err)
	}
	return ask.Status(name+" status", name, firstPositional(pos), *all, *format, "no "+name+" dispatches logged yet")
}

// firstPositional is pos[0], or "" when there is none.
func firstPositional(pos []string) string {
	if len(pos) == 0 {
		return ""
	}
	return pos[0]
}

// agentTail is `<v> tail [id] [-f]`: exit 0 when the job ended ok, 1 on error or a lost job.
func agentTail(name string, args []string) int {
	fs := newAgentFlags(name, "tail")
	follow := fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	return ask.TailJob(name+" tail", job.Record{Agent: name, ID: firstPositional(pos)}, helptext.Text, *follow)
}

// agentResult is `<v> result [id] [--format text|json]`.
func agentResult(name string, args []string) int {
	fs := newAgentFlags(name, "result")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" result", err)
	}
	return ask.PrintAgentResult(name, firstPositional(pos), *format)
}

// agentCancel is `<v> cancel [id]`.
func agentCancel(name string, args []string) int {
	fs := newAgentFlags(name, "cancel")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	return ask.CancelJob(name+" cancel", name, fs.Arg(0), helptext.Text)
}

// agentCatalog is `<v> catalog [--profile p] [--format f]`; only copilot takes flags.
func agentCatalog(name string, args []string) int {
	if opencode.IsMenuVerb(name, args) {
		return opencode.RunMenu(args)
	}
	if agentSubHelp(name, "catalog", args) {
		return 0
	}
	if name != "copilot" && len(args) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s catalog: unexpected argument %q\n", name, args[0])
		return 2
	}
	fs := newAgentFlags(name, "catalog")
	profile := fs.String("profile", "", "list this profile's models (default: the session profile)")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" catalog", err)
	}
	return ask.Catalog(name, *profile, *format, cmdModels)
}

// agentProfile is `<v> profile [<profile>] [--clear]`: show, set or clear the agent's record profile.
func agentProfile(name string, args []string) int {
	if _, ok := agent.LookupSchema(name); !ok {
		return helptext.Fail("%s profile: unknown agent", name)
	}
	fs := newAgentFlags(name, "profile")
	unpin := fs.Bool("clear", false, "unpin: launches use today's selection again")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 1 || (*unpin && len(pos) > 0) {
		return helptext.Fail("%s profile: want one profile name, or --clear", name)
	}
	if err := configure.AgentProfile(os.Stdout, name, firstPositional(pos), *unpin); err != nil {
		return helptext.Fail("%s profile: %v", name, err)
	}
	return 0
}

// agentOnOff is `<v> on|off`.
func agentOnOff(name string, on bool, args []string) int {
	verb := map[bool]string{true: "on", false: "off"}[on]
	fs := newAgentFlags(name, verb)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.Fail("%s %s: takes no arguments", name, verb)
	}
	if err := configure.SetAgentActive(name, on); err != nil {
		return helptext.Fail("%s %s: %v", name, verb, err)
	}
	fmt.Printf("%s: %s\n", name, verb)
	return 0
}

// cmdClaude is `claude-code claude`: Claude Code on the selected profile.
func cmdClaude(args []string) int { return claude.Launch("", args) }

// hookPage lists modules in Hint, not Commands: a COMMANDS row would invite `hook <name> -h`, which runs the hook.
var hookPage = helptext.Page{
	Usage: "claude-code hook <Event> [--<module>|--all] < payload.json",
	About: "Run one lifecycle hook on the event JSON on stdin; settings.json and the agent hook files invoke it.",
	Hint:  settings.HookModules() + "\nevents: <Event> runs the dispatcher, --<module> one module, --all every module",
}

// cmdHook runs one lifecycle hook.
func cmdHook(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		hookPage.Fprint(os.Stderr)
		return 0
	}
	return hooks.RunArgs(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr)
}

var anthropicPage = helptext.Page{
	Usage: "claude-code anthropic [--oauth[=NAME]] [--x-api-key[=NAME]] [--model M]\n" +
		"[--profile P] [--print-env] [--keep-env NAMES] [--overlay FILE] [args]\n" +
		"claude-code anthropic science [--print-env] [claude-science args]",
	About: "Claude Code on first-party Anthropic. No flag uses your claude.ai login. A bare flag takes no " +
		"value; =NAME picks the vault key. --profile runs on that registry profile instead.",
	Commands: [][2]string{{"science", "Claude for Life Sciences, own login (--print-env)"}},
	Hint:     "claude-code anthropic " + helptext.DocsFlag + " prints claude's own help.",
}

// anthropicHelp renders the page with its flag rows; the flags are parsed by claude.ParseAnthropicArgs.
func anthropicHelp(w io.Writer) {
	fs := helptext.NewFlagSet("anthropic", anthropicPage)
	fs.Bool("oauth", false, "setup-token login; =NAME picks the vault key")
	fs.Bool("x-api-key", false, "API key login; =NAME picks the vault key")
	fs.String("model", "", "opus|sonnet|haiku|fable, a version (opus-5-5) or wire id")
	fs.String("profile", "", "run on this registry profile instead")
	fs.Bool("print-env", false, "print the resolved launch; never prompts")
	fs.String("keep-env", "", "env names the credential scrub keeps")
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	p := anthropicPage
	p.Flags = fs
	p.Fprint(w)
}

// cmdAnthropic is `claude-code anthropic`: first-party Claude Code with a login, OAuth token or API key.
func cmdAnthropic(args []string) int {
	if len(args) > 0 && args[0] == "science" {
		return claude.LaunchScience(args[1:])
	}
	if len(args) > 0 && helptext.WantsHelp(args[:1]) { // only a leading help flag; `anthropic mcp --help` goes to claude
		anthropicHelp(os.Stdout)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		return helptext.WrappedDocs("claude")
	}
	return claude.LaunchAnthropic(args, func(name string) int { return cmdVault([]string{"set", name}) })
}
