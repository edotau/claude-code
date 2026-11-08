package agentcmd

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Subverbs are the words AgentVerb routes to agentSub: the keys of agentSubPages.
var Subverbs = func() map[string]bool {
	m := make(map[string]bool, len(agentSubPages))
	for verb := range agentSubPages {
		m[verb] = true
	}
	return m
}()

// agentAbout expands each agent's one-line menu summary for its help page.
var agentAbout = map[string]string{
	"copilot": "GitHub's Copilot CLI on its own login, or the selected Databricks profile. Bare " +
		"launches it with args " +
		"forwarded; the commands below run it headless, in the background or as a reviewer.",
	"codex": "OpenAI's Codex CLI on your ChatGPT login, the second-opinion reviewer. Bare " +
		"launches it with args forwarded; the commands below run it headless.",
	"gemini": "Gemini through agy (the Antigravity CLI) on your Google sign-in. Bare launches it " +
		"with args forwarded; the commands below run it headless (ask --dirs packs files).",
	"opencode": "The OpenCode CLI, a general code agent with the widest model pool (catalog), on its " +
		"own login or the selected Databricks profile. Bare launches it with args forwarded; the " +
		"commands below run it headless.",
}

// AgentPage is the help page for one agent verb: the shared command roster plus the CLI's extra rows.
func AgentPage(name string) helptext.Page {
	rows := agentCommandRows(name)
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

// agentCommandRows is the command roster every agent page starts from.
func agentCommandRows(name string) [][2]string {
	return [][2]string{
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

// firstPositional is pos[0], or "" when there is none.
func firstPositional(pos []string) string {
	if len(pos) == 0 {
		return ""
	}
	return pos[0]
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

// AgentVerb is the top-level spelling of an agent: bare = launch, `--` = forced passthrough,
// `ask` = `ask --agent <v>`, a word in Subverbs = agentSub, else launch with args forwarded.
func AgentVerb(name string, args []string) int {
	if len(args) == 0 {
		return LaunchAgent(name, args)
	}
	if args[0] == "--" {
		return LaunchAgent(name, args[1:])
	}
	if args[0] == "ask" {
		return ask.Cmd(append([]string{"--agent", name}, args[1:]...))
	}
	if Subverbs[args[0]] {
		return agentSub(name, args)
	}
	if len(args) == 1 && slices.Contains([]string{"-h", "--help", "help"}, args[0]) {
		AgentPage(name).Fprint(os.Stderr)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		s, _ := agent.LookupSchema(name)
		return helptext.WrappedDocs(s.CLI.Binary)
	}
	return LaunchAgent(name, args)
}

// agentSubHelp prints the sub-verb's page when args open with -h/--help, for sub-verbs without a FlagSet.
func agentSubHelp(name, verb string, args []string) bool {
	if len(args) == 0 || (args[0] != "-h" && args[0] != "--help") {
		return false
	}
	agentSubPage(name, verb).Fprint(os.Stderr)
	return true
}

// newAgentFlags makes a ContinueOnError FlagSet named "<agent> <verb>" whose -h renders the sub-verb's page.
func newAgentFlags(name, verb string) *flag.FlagSet {
	return helptext.NewFlagSet(name+" "+verb, agentSubPage(name, verb))
}

// agentSub dispatches an agent's background-job and setup subverbs (resume/status/result/cancel/setup/catalog).
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
