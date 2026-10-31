package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// agentSubverbs are the words AgentVerb routes to agentSub.
var agentSubverbs = map[string]bool{
	"resume": true, "status": true, "tail": true, "result": true, "cancel": true, "setup": true, "catalog": true,
	"review": true, "transfer": true, "explore": true, "profile": true, "on": true, "off": true,
}

// agentAbout expands each agent's one-line menu summary for its help page.
var agentAbout = map[string]string{
	"copilot": "GitHub's Copilot CLI, billed to your Copilot plan. Bare launches it with args " +
		"forwarded; the commands below run it headless, in the background or as a reviewer.",
	"codex": "OpenAI's Codex CLI on your ChatGPT login, the second-opinion reviewer. Bare " +
		"launches it with args forwarded; the commands below run it headless.",
	"gemini": "Gemini through agy (the Antigravity CLI) on your Google sign-in. Bare launches it " +
		"with args forwarded; bridge inlines whole directories into one 1M-context call.",
	"opencode": "The OpenCode CLI on your Copilot plan, for docs and drafting. Bare launches it " +
		"with args forwarded; the commands below run it headless.",
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

// agentSubPages maps each sub-verb to its synopsis tail and blurb; %s is the agent name.
var agentSubPages = map[string][2]string{
	"resume": {"resume <id> [ask flags] <task>",
		"Continue session <id> on %s (= ask --agent %[1]s --session <id>); ask's flags apply, see claude-code ask -h."},
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
	switch args[0] {
	case "resume":
		return agentResume(name, args[1:])
	case "status":
		return agentStatus(name, args[1:])
	case "tail":
		return agentTail(name, args[1:])
	case "result":
		return agentResult(name, args[1:])
	case "cancel":
		return agentCancel(name, args[1:])
	case "setup":
		return agentSetup(name, args[1:])
	case "catalog":
		if isOpencodeMenuVerb(name, args[1:]) {
			return cmdOpencodeMenu(args[1:])
		}
		if agentSubHelp(name, "catalog", args[1:]) {
			return 0
		}
		return agentModels(name, args[1:])
	case "review":
		return agentReview(name, args[1:])
	case "explore":
		return agentExplore(name, args[1:])
	case "transfer":
		return agentTransfer(name, args[1:])
	case "profile":
		return cmdAgentProfile(name, args[1:])
	case "on", "off":
		return cmdAgentOnOff(name, args[0] == "on", args[1:])
	}
	return fail("%s: unknown subcommand %q", name, args[0])
}

// agentResume is `claude-code <v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
func agentResume(name string, args []string) int {
	if agentSubHelp(name, "resume", args) {
		return 0
	}
	if len(args) < 1 {
		return fail("%s resume: usage: claude-code %s resume <id> <task>", name, name)
	}
	if strings.HasPrefix(args[0], "-") { // flags first: ask's own parser, the session id is its first positional
		return runAsk(append([]string{"--agent", name}, args...), true)
	}
	return cmdAsk(append([]string{"--agent", name, "--session", args[0]}, args[1:]...))
}

// agentStatus is `claude-code <v> status [id] [--all] [--format text|json]`.
func agentStatus(name string, args []string) int {
	fs := newAgentFlags(name, "status")
	all := fs.Bool("all", false, "every repo, not just this one")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat(name+" status", err)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	cwd, _ := os.Getwd()
	rows, err := agent.Status(context.Background(), name, cwd, ref, *all)
	if err != nil {
		return fail("%s status: %v", name, err)
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rows); err != nil {
			return fail("%s status: %v", name, err)
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Printf("no %s dispatches logged yet\n", name)
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, statusDetail(r))
	}
	return 0
}

// agentTail is `claude-code <v> tail [id] [-f]`: exit 0 when the job ended ok, 1 on error or a lost job.
func agentTail(name string, args []string) int {
	fs := newAgentFlags(name, "tail")
	follow := fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, _ := os.Getwd()
	rec, err := agent.Tail(ctx, name, cwd, ref, *follow, os.Stdout)
	if err != nil {
		return fail("%s tail: %v", name, err)
	}
	return tailExit(rec, *follow)
}

// tailExit is a tail's exit code: 1 for an errored job, or for a followed job that is still "running" (lost).
func tailExit(rec agent.Record, follow bool) int {
	switch {
	case rec.Status == "error":
		fmt.Fprintf(os.Stderr, "error: %s\n", shapes.Redact(rec.Error)) // the stderr tail in it may carry a key
		return 1
	case rec.Status == "running" && follow:
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	return 0
}

// agentResult is `claude-code <v> result [id] [--format text|json]`.
func agentResult(name string, args []string) int {
	fs := newAgentFlags(name, "result")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat(name+" result", err)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	cwd, _ := os.Getwd()
	rec, log, err := agent.JobResult(context.Background(), name, cwd, ref)
	if err != nil {
		return fail("%s result: %v", name, err)
	}
	s, _ := agent.LookupSchema(name)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			ID      string `json:"id"`
			Session string `json:"session,omitempty"`
			Resume  string `json:"resume,omitempty"`
			Log     string `json:"log"`
		}{rec.ID, rec.SessionID, s.ResumeHint(rec.SessionID), log}); err != nil {
			return fail("%s result: %v", name, err)
		}
		return resultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: %s\n", s.ResumeHint(rec.SessionID))
	}
	return resultExit(rec)
}

// resultExit reports a failed job's error on stderr and returns 1; a finished ok job returns 0.
func resultExit(rec agent.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", rec.Error)
	return 1
}

// agentCancel is `claude-code <v> cancel [id]`.
func agentCancel(name string, args []string) int {
	fs := newAgentFlags(name, "cancel")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ref := fs.Arg(0)
	cwd, _ := os.Getwd()
	rec, err := agent.CancelJob(context.Background(), name, cwd, ref)
	if err != nil {
		return fail("%s cancel: %v", name, err)
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// agentSetup is `claude-code <v> setup [--format text|json]`.
func agentSetup(name string, args []string) int {
	fs := newAgentFlags(name, "setup")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat(name+" setup", err)
	}
	row, ok := agent.LookupSchema(name)
	if !ok {
		row = agent.Schema{Name: name} // reported as an unknown agent, by name
	}
	rep := row.Setup()
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rep); err != nil {
			return fail("%s setup: %v", name, err)
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
func agentReview(name string, args []string) int {
	fs := newAgentFlags(name, "review")
	uncommitted := fs.Bool("uncommitted", false, "working tree + index vs HEAD (default)")
	base := fs.String("base", "", "diff against this branch (triple-dot merge base)")
	commit := fs.String("commit", "", "diff this single commit")
	pr := fs.Bool("pr", false, "diff against the remote's default branch")
	adversarial := fs.Bool("adversarial", false, "adversarial findings instead of a neutral verdict")
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
	rubric := fs.String("rubric", "", "grade the review against this rubric file")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	scope, err := agent.ParseScope(*uncommitted, *base, *commit, *pr)
	if err != nil {
		return fail("%s review: %v", name, err)
	}
	cwd, _ := os.Getwd()
	diff, err := agent.Diff(context.Background(), cwd, scope)
	if err != nil {
		return fail("%s review: %v", name, err)
	}
	focus := strings.Join(fs.Args(), " ")
	if parts := agent.ReviewParts(diff, *adversarial, focus, *rubric); len(parts) > 1 {
		return runSplitReview(name, args, parts, scope, *adversarial, *background, focus, *rubric)
	}
	askArgs, cleanup, err := agent.ReviewArgs(name, scope, diff, *adversarial, *background, focus, *rubric)
	if err != nil {
		return fail("%s review: %v", name, err)
	}
	defer cleanup()
	return cmdAsk(askArgs)
}

// agentExplore is `claude-code <v> explore [--no-web] [--background] [--format f] <question>`.
func agentExplore(name string, args []string) int {
	fs := newAgentFlags(name, "explore")
	noWeb := fs.Bool("no-web", false, "skip the web-research leg; code scan only")
	background := fs.Bool("background", false, "detach; claude-code "+name+" status|result to follow up")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	question := strings.Join(fs.Args(), " ")
	if question == "" {
		return fail("%s explore: usage: claude-code %s explore [--no-web] [--background] <question>", name, name)
	}
	cwd, _ := os.Getwd()
	askArgs := agent.ExploreArgs(name, cwd, question, !*noWeb, *background)
	if *format != "text" {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", *format, task)
	}
	return cmdAsk(askArgs)
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
	src := *source
	if src != "" {
		if err := agent.ValidateSource(src); err != nil {
			return fail("%s transfer: %v", name, err)
		}
	} else {
		var err error
		src, err = agent.DefaultSource(cwd)
		if err != nil {
			return fail("%s transfer: %v", name, err)
		}
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	note := strings.Join(fs.Args(), " ")
	prompt, err := agent.Handoff(src, cwd, note)
	if err != nil {
		return fail("%s transfer: %v", name, err)
	}
	code := cmdAsk(agent.TransferArgs(name, cwd, prompt, *wait))
	if code == 0 && *wait {
		if rec, _, err := agent.JobResult(context.Background(), name, cwd, ""); err == nil {
			s, _ := agent.LookupSchema(name)
			if hint := s.ResumeHint(rec.SessionID); hint != "" {
				fmt.Printf("resume: %s\n", hint)
			}
		}
	}
	return code
}

// agentModels is `claude-code <v> catalog` : the agent's own catalog, or a note that it has none.
func agentModels(name string, args []string) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return fail("unknown agent %q", name)
	}
	format := helptext.Text
	if name == "copilot" {
		fs := newAgentFlags(name, "catalog")
		profile := fs.String("profile", "", "list this profile's models (default: the session profile)")
		f := helptext.FormatFlag(fs)
		if code, done := helptext.Parse(fs, args); done {
			return code
		}
		if err := helptext.CheckFormat(*f); err != nil {
			return badFormat(name+" catalog", err)
		}
		format = *f
		reg, err := providers.Load()
		if err != nil {
			return fail("%v", err)
		}
		cat, err := agent.CopilotModelSource(context.Background(), reg, *profile)
		if err != nil {
			return fail("%s catalog: %v", name, err)
		}
		if cat.Databricks {
			return printCopilotCatalog(name, cat, format)
		}
		if format == helptext.Text {
			fmt.Println(cat.Source)
		}
	} else if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "error: %s catalog: unexpected argument %q\n", name, args[0])
		return 2
	}
	switch {
	case s.CLI.ModelsProvider != "":
		reg, err := providers.Load()
		if err != nil {
			return fail("%v", err)
		}
		n, err := s.ModelsProfile(reg)
		if err != nil {
			return fail("%v", err)
		}
		return cmdModels([]string{"--live", "--profile", n, "--format", format})
	case len(s.CLI.ModelsArgv) > 0:
		return launchAgent(name, s.CLI.ModelsArgv)
	default:
		fmt.Printf("%s: no model catalog\n", name)
		return 0
	}
}

// printCopilotCatalog prints a Databricks row's chat endpoints as a NAME / TESTED table, or JSON.
func printCopilotCatalog(name string, cat agent.CopilotCatalog, format string) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, cat); err != nil {
			return fail("%s catalog: %v", name, err)
		}
		return 0
	}
	fmt.Println(cat.Source)
	w := len("NAME")
	for _, r := range cat.Rows {
		w = max(w, len(r.ID))
	}
	fmt.Printf("%-*s  %s\n", w, "NAME", "TESTED")
	for _, r := range cat.Rows {
		mark := ""
		if r.Tested {
			mark = "tested"
		}
		fmt.Println(strings.TrimRight(fmt.Sprintf("%-*s  %s", w, r.ID, mark), " "))
	}
	return 0
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
	if name == "codex" {
		fs.String("f", "", "config.toml whose keys ride as -c overrides")
	}
	return fs
}

// statusDetail is a status row's tail: progress and preview, or the preview plus the one-line error text.
func statusDetail(r agent.Row) string {
	d := r.Preview
	if r.Progress != "" {
		d = "[" + r.Progress + "] " + d
	}
	if r.Error != "" {
		d += " -- " + agent.Preview(r.Error, 300)
	}
	return d
}
