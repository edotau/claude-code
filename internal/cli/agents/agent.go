package agents

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagepkg "github.com/edotau/claude-code/internal/usage"
)

// RouteRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var RouteRefresh = usagepkg.RefreshAccount

// attachFlag collects repeatable --attach paths; Set resolves each to an absolute path and fails fast
// unless it names an existing regular file.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

// Placeholder names the FLAGS placeholder for attachFlag (helptext.Placeholder).
func (a *attachFlag) Placeholder() string { return "PATH" }

func (a *attachFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--attach %q: not a regular file", s)
	}
	// reject non-image binary up front: it fails the same way on every agent, so it must never reach the fallback chain
	if data, err := os.ReadFile(abs); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(data) && !agent.IsImage(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
}

// keepEnvFlag collects repeatable --keep-env NAME[,NAME] values, comma-split per occurrence.
type keepEnvFlag []string

func (k *keepEnvFlag) String() string { return strings.Join(*k, ",") }

// Placeholder names the FLAGS placeholder for keepEnvFlag (helptext.Placeholder).
func (k *keepEnvFlag) Placeholder() string { return "NAME,NAME" }

func (k *keepEnvFlag) Set(s string) error {
	*k = append(*k, strings.Split(s, ",")...)
	return nil
}

// envFlag collects repeatable --env NAME=VALUE entries.
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

// Placeholder names the FLAGS placeholder for envFlag (helptext.Placeholder).
func (e *envFlag) Placeholder() string { return "NAME=VALUE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so RunAsk validates after Parse.
func (e *envFlag) Set(s string) error {
	*e = append(*e, s)
	return nil
}

// agentsFlag is the --agents value: inline JSON or a file of agent definitions.
type agentsFlag string

func (a *agentsFlag) String() string { return string(*a) }

// Placeholder names the FLAGS placeholder for agentsFlag (helptext.Placeholder).
func (a *agentsFlag) Placeholder() string { return "JSON|FILE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so RunAsk validates after Parse.
func (a *agentsFlag) Set(s string) error {
	*a = agentsFlag(s)
	return nil
}

// NewClient builds the dispatch client; RouteRefresh is the offline-able account refresh.
func NewClient() *agent.Client {
	return agent.NewClient(router.RuleClassifier{}.Alias, RouteRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}

// backgroundExtraFlags carries explicit routing and identity flags over to the spawned --job child.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "as":
			extra = append(extra, "--as", f.Value.String())
		case "fallback":
			extra = append(extra, "--fallback", fallback)
		case "native":
			if native {
				extra = append(extra, "--native")
			}
		}
	})
	return extra
}

// ReportBackgroundStart tells the caller how to track a detached job.
func ReportBackgroundStart(format string, rec agent.Record) {
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
		fmt.Println(string(body))
		return
	}
	fmt.Printf("job %s started — claude-code agents status|tail|result|cancel %s\n", rec.ID, rec.ID)
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	helptext.Fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json.
func printAskFailure(format, agentName string, err error) {
	switch format {
	case helptext.JSON:
		body, _ := json.Marshal(agent.FailureEnvelope(agentName, err))
		fmt.Println(string(body))
	case formatStreamJSON:
		_ = askStdoutSink(format, "").Emit(agent.ResultEvent(agent.FailureEnvelope(agentName, err)))
	}
}

func reportAskResult(format string, res agent.Result, err error, dispatchID string) {
	for _, d := range res.Dropped {
		if d == "--read-only (tools off)" {
			fmt.Fprintf(os.Stderr, "ask: the %s agent runs with tools off (read-only sandbox where it has one)\n", res.Agent)
			continue
		}
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == helptext.JSON {
		body, _ := json.Marshal(agent.NewEnvelope(res, err, dispatchID))
		fmt.Println(string(body))
		return
	}
	if format == formatStreamJSON {
		return // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
}

// agentsPage is `agents -h`; bare `agents` lists the agents.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "With no command, list each agent, whether it is available and which " +
		"request fields (model, profile, effort, …) it honors.",
	Commands: [][2]string{
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

// agentsSyncPage is `agents sync -h`.
var agentsSyncPage = helptext.Page{
	Usage: "claude-code agents sync [flags]",
	About: "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills.",
}

// agentsReapPage is `agents reap -h`.
var agentsReapPage = helptext.Page{
	Usage: "claude-code agents reap [flags]",
	About: "Mark dispatch records still \"running\" whose process is gone as error, and drop stale " +
		"agent-inflight markers.",
}

func CmdAgents(args []string) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !slices.Contains([]string{"-h", "--help"}, args[0])) {
		return cmdAgentsList(args)
	}
	switch args[0] {
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
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return project.Sync(project.Options{DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Agent: *name, Out: os.Stdout})
}

// cmdAgentsList is bare `agents`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := helptext.NewFlagSet("agents", agentsPage)
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents --format)")
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

// ApplyRoute exports --route as HARNESS_ROUTE, so this process's picks (and the jobs it spawns) follow that ladder.
func ApplyRoute(route string) error {
	if route == "" {
		return nil
	}
	if !slices.Contains(router.RouteModes, route) {
		return fmt.Errorf("--route %q unknown (%s)", route, strings.Join(router.RouteModes, "|"))
	}
	return os.Setenv(router.EnvRoute, route)
}
