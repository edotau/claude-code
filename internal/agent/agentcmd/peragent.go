package agentcmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/providers/providerscmd"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
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

// SplitReview runs a multi-part review; the caller owns it until the review verbs move here.
type SplitReview func(name string, args []string, parts []roles.Part, scope roles.Scope,
	adversarial, background bool, focus, rubric, model string) int

// AgentVerb is the top-level spelling of an agent: bare = launch, `--` = forced passthrough,
// `ask` = `ask --agent <v>`, a word in Subverbs = agentSub, else launch with args forwarded.
func AgentVerb(name string, args []string, split SplitReview) int {
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
		return agentSub(name, args, split)
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
func agentSub(name string, args []string, split SplitReview) int {
	handlers := map[string]func(string, []string) int{
		"resume": agentResume, "status": agentStatus, "tail": agentTail,
		"result": agentResult, "cancel": agentCancel, "setup": agentSetup,
		"catalog": agentCatalog, "explore": agentExplore,
		"transfer": agentTransfer, "profile": cmdAgentProfile,
		"review": func(name string, args []string) int { return agentReview(name, args, split) },
		"on":     func(name string, args []string) int { return cmdAgentOnOff(name, true, args) },
		"off":    func(name string, args []string) int { return cmdAgentOnOff(name, false, args) },
	}
	if run := handlers[args[0]]; run != nil {
		return run(name, args[1:])
	}
	return helptext.Fail("%s: unknown subcommand %q", name, args[0])
}

func agentCatalog(name string, args []string) int {
	if opencode.IsMenuVerb(name, args) {
		return opencode.RunMenu(args)
	}
	if agentSubHelp(name, "catalog", args) {
		return 0
	}
	return agentModels(name, args)
}

// agentResume is `claude-code <v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
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

// printResumeHint prints the agent's resume command for a session; nothing when it has none (ResumeHint("") is "").
func printResumeHint(s agent.Schema, sessionID string) {
	if hint := s.ResumeHint(sessionID); hint != "" {
		fmt.Printf("resume: %s\n", hint)
	}
}

// agentSetup is `claude-code <v> setup [--format text|json]`.
func agentSetup(name string, args []string) int {
	fs := newAgentFlags(name, "setup")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat(name+" setup", err)
	}
	row, ok := agent.LookupSchema(name)
	if !ok {
		row = agent.Schema{Name: name} // reported as an unknown agent, by name
	}
	rep := row.Setup()
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return helptext.Fail("%s setup: %v", name, err)
		}
	} else {
		for _, c := range rep.Checks {
			fmt.Printf("%s  %s  %s  %s\n", rep.Agent, c.Name, c.State, c.Detail)
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// agentReview is `claude-code <v> review [scope flags] [--adversarial] [--background] [--rubric f] [focus…]`.
func agentReview(name string, args []string, split SplitReview) int {
	fs := newAgentFlags(name, "review")
	uncommitted := fs.Bool("uncommitted", false, "working tree + index vs HEAD (default)")
	base := fs.String("base", "", "diff against this branch (triple-dot merge base)")
	commit := fs.String("commit", "", "diff this single commit")
	pr := fs.Bool("pr", false, "diff against the remote's default branch")
	adversarial := fs.Bool("adversarial", false, "adversarial findings instead of a neutral verdict")
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
	rubric := fs.String("rubric", "", "grade the review against this rubric file")
	model := fs.String("model", "", "model for the review (default: the agent's pin)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	scope, err := roles.ParseScope(*uncommitted, *base, *commit, *pr)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	cwd, _ := os.Getwd()
	diff, err := roles.Diff(context.Background(), cwd, scope)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	focus := strings.Join(fs.Args(), " ")
	if parts := roles.ReviewParts(diff, *adversarial, focus, *rubric); len(parts) > 1 {
		return split(name, args, parts, scope, *adversarial, *background, focus, *rubric, *model)
	}
	askArgs, cleanup, err := roles.ReviewArgs(name, scope, diff, *adversarial, *background, focus, *rubric, *model)
	if err != nil {
		return helptext.Fail("%s review: %v", name, err)
	}
	defer cleanup()
	return ask.Cmd(askArgs)
}

// agentExplore is `claude-code <v> explore [--no-web] [--background] [--format f] <question>`.
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
	cwd, _ := os.Getwd()
	askArgs := roles.Args(name, cwd, question, !*noWeb, *background)
	if *format != "text" {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", *format, task)
	}
	return ask.Cmd(askArgs)
}

// agentTransfer is `claude-code <v> transfer [--source f] [--wait] [note…]`.
func agentTransfer(name string, args []string) int {
	fs := newAgentFlags(name, "transfer")
	source := fs.String("source", "", "transcript .jsonl (default: newest session for this repo)")
	wait := fs.Bool("wait", false, "block for the answer instead of detaching")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	cwd, _ := os.Getwd()
	src, err := transcript.ResolveSource(*source, cwd)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	note := strings.Join(fs.Args(), " ")
	prompt, err := transcript.Handoff(src, cwd, note)
	if err != nil {
		return helptext.Fail("%s transfer: %v", name, err)
	}
	code := ask.Cmd(transcript.TransferArgs(name, cwd, prompt, *wait))
	if code == 0 && *wait {
		if rec, _, err := job.JobResult(context.Background(), name, cwd, ""); err == nil {
			s, _ := agent.LookupSchema(name)
			printResumeHint(s, rec.SessionID)
		}
	}
	return code
}

// agentModels is `claude-code <v> catalog` : the agent's own catalog, or a note that it has none.
func agentModels(name string, args []string) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	format := helptext.Text
	if name == "copilot" {
		var code int
		var done bool
		format, code, done = copilotCatalog(args)
		if done {
			return code
		}
	} else if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s catalog: unexpected argument %q\n", name, args[0])
		return 2
	}
	return launchAgentCatalog(name, s, format)
}

func copilotCatalog(args []string) (string, int, bool) {
	name := "copilot"
	fs := newAgentFlags(name, "catalog")
	profile := fs.String("profile", "", "list this profile's models (default: the session profile)")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return "", code, true
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return "", helptext.BadFormat(name+" catalog", err), true
	}
	reg, err := providers.Load()
	if err != nil {
		return "", helptext.Fail("%v", err), true
	}
	cat, err := copilot.CopilotModelSource(context.Background(), reg, *profile)
	if err != nil {
		return "", helptext.Fail("%s catalog: %v", name, err), true
	}
	if cat.Databricks {
		if err := cat.Write(os.Stdout, *format); err != nil {
			return "", helptext.Fail("%s catalog: %v", name, err), true
		}
		return "", 0, true
	}
	if *format == helptext.Text {
		fmt.Println(cat.Source)
	}
	return *format, 0, false
}

func launchAgentCatalog(name string, s agent.Schema, format string) int {
	switch {
	case s.CLI.ModelsProvider != "":
		reg, err := providers.Load()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		n, err := s.ModelsProfile(reg)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return providerscmd.Models([]string{"--live", "--profile", n, "--format", format})
	case len(s.CLI.ModelsArgv) > 0:
		return LaunchAgent(name, s.CLI.ModelsArgv)
	default:
		fmt.Printf("%s: no model catalog\n", name)
		return 0
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
	fs.String("overlay", "", "JSON {set,unset,keep} file over the child's env")
	if name == "codex" {
		fs.String("f", "", "config.toml whose keys ride as -c overrides")
	}
	return fs
}

// cmdAgentProfile is `<agent> profile [<profile>] [--clear]`: show, set or clear the agent's record profile.
func cmdAgentProfile(name string, args []string) int {
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
	switch {
	case *unpin:
		own := vault.OwnLoginProfiles[name] // an external agent's cleared profile is its own login, not off
		if _, err := configure.ApplyProfile([]string{name}, own); err != nil {
			return helptext.Fail("%s profile: %v", name, err)
		}
		fmt.Printf("%s: profile cleared (own login)\n", name)
		return 0
	case len(pos) == 0:
		fmt.Println(configure.ProfileStatus(name))
		return 0
	}
	return setAgentProfile(name, pos[0])
}

func setAgentProfile(name, profile string) int {
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%s profile: %v", name, err)
	}
	if _, err := configure.ApplyProfile([]string{name}, profile); err != nil {
		return helptext.Fail("%s profile: %v (see claude-code configure provider --list)", name, err)
	}
	fmt.Printf("%s: now on %s; relaunch `claude-code %s` to apply\n", name, profile, name)
	preflight.Warm(os.Stdout, reg, time.Now())
	if name == "codex" {
		fmt.Println("plain codex: run `claude-code configure --restore --agents` to re-render config.toml")
	}
	return 0
}

// cmdAgentOnOff is `<agent> on|off`: off clears the record's profile, on sets the own login; claude is always on.
func cmdAgentOnOff(name string, on bool, args []string) int {
	verb, value := "off", agent.AgentOff
	if on {
		verb, value = "on", agent.AgentOn
	}
	fs := newAgentFlags(name, verb)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.Fail("%s %s: takes no arguments", name, verb)
	}
	if agent.VaultAgentIsClaude(name) {
		return helptext.Fail("%s %s: %s is always on", name, verb, name)
	}
	if err := agent.ValidAgentActive(name, value); err != nil {
		return helptext.Fail("%s %s: %v", name, verb, err)
	}
	profile := ""
	if on {
		profile = vault.OwnLoginProfiles[name]
	}
	if _, err := configure.ApplyProfile([]string{name}, profile); err != nil {
		return helptext.Fail("%s %s: %v", name, verb, err)
	}
	if on {
		fmt.Printf("%s: on\n", name)
	} else {
		fmt.Printf("%s: off\n", name)
	}
	return 0
}
