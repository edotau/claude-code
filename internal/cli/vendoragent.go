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
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets/shapes"
	"github.com/edotau/claude-code/internal/vendoragent"
	"github.com/edotau/claude-code/internal/vendors"
)

// vendorSubverbs are the words vendorVerb routes to cmdVendorSub.
var vendorSubverbs = map[string]bool{
	"resume": true, "status": true, "tail": true, "result": true, "cancel": true, "setup": true, "models": true,
	"review": true, "transfer": true, "explore": true, "env": true,
}

// vendorAbout expands each vendor's one-line menu summary for its help page.
var vendorAbout = map[string]string{
	"copilot": "GitHub's Copilot CLI, billed to your Copilot plan. Bare launches it with args " +
		"forwarded; the commands below run it headless, in the background or as a reviewer.",
	"codex": "OpenAI's Codex CLI on your ChatGPT login, the second-opinion reviewer. Bare " +
		"launches it with args forwarded; the commands below run it headless.",
	"gemini": "Gemini through agy (the Antigravity CLI) on your Google sign-in. Bare launches it " +
		"with args forwarded; bridge inlines whole directories into one 1M-context call.",
	"opencode": "The OpenCode CLI on your Copilot plan, for docs and drafting. Bare launches it " +
		"with args forwarded; the commands below run it headless.",
}

// vendorPage is the help page for one vendor verb: the shared command roster plus the adapter's extra rows.
func vendorPage(vendor string) helptext.Page {
	rows := [][2]string{
		{"ask", "one-shot run (= ask --agent " + vendor + ")"},
		{"resume", "continue a session"},
		{"review", "diff review; --adversarial"},
		{"explore", "scan code + web research → brief"},
		{"transfer", "hand this Claude session to " + vendor + "; --source"},
		{"status", "background jobs in this repo; --all"},
		{"tail", "a job's output; -f follows live"},
		{"result", "a finished job's answer + session"},
		{"cancel", "interrupt a running job"},
		{"setup", "binary, login, route, projection; --format json"},
		{"models", "the vendor's own model catalog"},
		{"env", "show or move only " + vendor + "'s account: dev, qa, prod"},
	}
	if vendor == "opencode" {
		rows[slices.IndexFunc(rows, func(r [2]string) bool { return r[0] == "models" })][1] =
			"model catalog; list|add|remove|import|reset edit the menu"
	}
	if a, ok := vendoragent.Lookup(vendor); ok {
		rows = append(rows, a.ExtraUsage...)
	}
	return helptext.Page{
		Usage:    "claude-code " + vendor + " [<command>|--] [launch flags] [args…]",
		About:    vendorAbout[vendor],
		Commands: rows,
		Flags:    vendorLaunchFlags(vendor),
		Hint: "claude-code " + vendor + " <command> -h lists that command's flags.\n" +
			"claude-code " + vendor + " " + helptext.DocsFlag + " prints " + vendorBinary(vendor) + "'s own help.",
	}
}

// vendorSubPages maps each sub-verb to its synopsis tail and blurb; %s is the vendor name.
var vendorSubPages = map[string][2]string{
	"resume": {"resume <id> [ask flags] <task>",
		"Continue session <id> on %s (= ask --agent %[1]s --session <id>); ask's flags apply, see claude-code ask -h."},
	"status":   {"status [id] [flags]", "Background %s jobs in this repo, newest first; an id narrows to one job."},
	"tail":     {"tail [id] [flags]", "A job's output (default: the newest). Exits 1 when the job failed or was lost."},
	"result":   {"result [id] [flags]", "A finished job's answer (default: the newest), its session and resume command."},
	"cancel":   {"cancel", "Interrupt a running %s job (default: the single running job in this repo)."},
	"setup":    {"setup [flags]", "Check %s's binary, login, route and projection; exits 1 when one fails."},
	"models":   {"models", "The %s CLI's own model catalog, or a note that it has none."},
	"review":   {"review [flags] [focus…]", "Review a diff with %s; trailing words name the focus."},
	"explore":  {"explore [flags] <question>", "Scan the code and research the web with %s, then return a brief."},
	"env":      {"env [dev|qa|prod]", "Show %s's account, or move only %[1]s to another env of its preset."},
	"transfer": {"transfer [flags] [note…]", "Hand this Claude session's transcript to %s, detached unless --wait."},
}

// vendorSubPage is the help page for `<vendor> <verb>`.
func vendorSubPage(vendor, verb string) helptext.Page {
	sp := vendorSubPages[verb]
	about := sp[1]
	if strings.Contains(about, "%") {
		about = fmt.Sprintf(about, vendor)
	}
	page := helptext.Page{Usage: "claude-code " + vendor + " " + sp[0], About: about}
	if verb == "env" {
		for _, env := range providers.DatabricksEnvs {
			page.Commands = append(page.Commands, [2]string{env, "move " + vendor + " to the " + env + " workspace"})
		}
	}
	return page
}

// vendorSubHelp prints the sub-verb's page when args open with -h/--help, for sub-verbs without a FlagSet.
func vendorSubHelp(vendor, verb string, args []string) bool {
	if len(args) == 0 || (args[0] != "-h" && args[0] != "--help") {
		return false
	}
	vendorSubPage(vendor, verb).Fprint(os.Stderr)
	return true
}

// newVendorFlags makes a ContinueOnError FlagSet named "<vendor> <verb>" whose -h renders the sub-verb's page.
func newVendorFlags(vendor, verb string) *flag.FlagSet {
	return helptext.NewFlagSet(vendor+" "+verb, vendorSubPage(vendor, verb))
}

// cmdVendorSub dispatches a vendor's background-job and setup subverbs (resume/status/result/cancel/setup/models).
func cmdVendorSub(vendor string, args []string) int {
	switch args[0] {
	case "resume":
		return cmdVendorResume(vendor, args[1:])
	case "status":
		return cmdVendorStatus(vendor, args[1:])
	case "tail":
		return cmdVendorTail(vendor, args[1:])
	case "result":
		return cmdVendorResult(vendor, args[1:])
	case "cancel":
		return cmdVendorCancel(vendor, args[1:])
	case "setup":
		return cmdVendorSetup(vendor, args[1:])
	case "models":
		if isOpencodeMenuVerb(vendor, args[1:]) {
			return cmdOpencodeMenu(args[1:])
		}
		if vendorSubHelp(vendor, "models", args[1:]) {
			return 0
		}
		return cmdVendorModels(vendor)
	case "review":
		return cmdVendorReview(vendor, args[1:])
	case "explore":
		return cmdVendorExplore(vendor, args[1:])
	case "transfer":
		return cmdVendorTransfer(vendor, args[1:])
	case "env":
		if slices.Contains(args[1:], "-h") || slices.Contains(args[1:], "--help") { // `env prod -h` too
			vendorSubPage(vendor, "env").Fprint(os.Stderr)
			return 0
		}
		return cmdVendorEnv(vendor, args[1:])
	}
	return fail("%s: unknown subcommand %q", vendor, args[0])
}

// cmdVendorResume is `claude-code <v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
func cmdVendorResume(vendor string, args []string) int {
	if vendorSubHelp(vendor, "resume", args) {
		return 0
	}
	if len(args) < 1 {
		return fail("%s resume: usage: claude-code %s resume <id> <task>", vendor, vendor)
	}
	return cmdAsk(append([]string{"--agent", vendor, "--session", args[0]}, args[1:]...))
}

// cmdVendorStatus is `claude-code <v> status [id] [--all] [--format text|json]`.
func cmdVendorStatus(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "status")
	all := fs.Bool("all", false, "every repo, not just this one")
	format := outfmt.Flag(fs)
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%s status: %v", vendor, err)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	cwd, _ := os.Getwd()
	rows, err := vendoragent.Status(context.Background(), vendor, cwd, ref, *all)
	if err != nil {
		return fail("%s status: %v", vendor, err)
	}
	if *format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, rows); err != nil {
			return fail("%s status: %v", vendor, err)
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Printf("no %s dispatches logged yet\n", vendor)
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, r.Preview)
	}
	return 0
}

// cmdVendorTail is `claude-code <v> tail [id] [-f]`: exit 0 when the job ended ok, 1 on error or a lost job.
func cmdVendorTail(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "tail")
	follow := fs.Bool("f", false, "follow until the job finishes, with a heartbeat on silence")
	pos, err := outfmt.Parse(fs, args)
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
	rec, err := vendoragent.Tail(ctx, vendor, cwd, ref, *follow, os.Stdout)
	if err != nil {
		return fail("%s tail: %v", vendor, err)
	}
	switch {
	case rec.Status == "error":
		fmt.Fprintf(os.Stderr, "error: %s\n", shapes.Redact(rec.Error)) // the stderr tail in it may carry a key
		return 1
	case rec.Status == "running" && *follow:
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	return 0
}

// cmdVendorResult is `claude-code <v> result [id] [--format text|json]`.
func cmdVendorResult(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "result")
	format := outfmt.Flag(fs)
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%s result: %v", vendor, err)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	cwd, _ := os.Getwd()
	rec, log, err := vendoragent.Result(context.Background(), vendor, cwd, ref)
	if err != nil {
		return fail("%s result: %v", vendor, err)
	}
	if *format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, struct {
			ID      string `json:"id"`
			Session string `json:"session,omitempty"`
			Resume  string `json:"resume,omitempty"`
			Log     string `json:"log"`
		}{rec.ID, rec.SessionID, resumeHint(vendor, rec.SessionID), log}); err != nil {
			return fail("%s result: %v", vendor, err)
		}
		return resultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: %s\n", resumeHint(vendor, rec.SessionID))
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

// resumeHint renders Adapter.ResumeHint for id, empty when the vendor has none.
func resumeHint(vendor, id string) string {
	a, ok := vendoragent.Lookup(vendor)
	if !ok || a.ResumeHint == nil || id == "" {
		return ""
	}
	return a.ResumeHint(id)
}

// cmdVendorCancel is `claude-code <v> cancel [id]`.
func cmdVendorCancel(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "cancel")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ref := fs.Arg(0)
	cwd, _ := os.Getwd()
	rec, err := vendoragent.CancelJob(context.Background(), vendor, cwd, ref)
	if err != nil {
		return fail("%s cancel: %v", vendor, err)
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// cmdVendorSetup is `claude-code <v> setup [--format text|json]`.
func cmdVendorSetup(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "setup")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%s setup: %v", vendor, err)
	}
	rep := vendoragent.Setup(vendor)
	if *format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, rep); err != nil {
			return fail("%s setup: %v", vendor, err)
		}
	} else {
		for _, c := range rep.Checks {
			fmt.Printf("%s  %s  %s  %s\n", rep.Vendor, c.Name, c.State, c.Detail)
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

// cmdVendorReview is `claude-code <v> review [scope flags] [--adversarial] [--background] [--rubric f] [focus…]`.
func cmdVendorReview(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "review")
	uncommitted := fs.Bool("uncommitted", false, "working tree + index vs HEAD (default)")
	base := fs.String("base", "", "diff against this branch (triple-dot merge base)")
	commit := fs.String("commit", "", "diff this single commit")
	pr := fs.Bool("pr", false, "diff against the remote's default branch")
	adversarial := fs.Bool("adversarial", false, "adversarial findings instead of a neutral verdict")
	background := fs.Bool("background", false, "detach; claude-code "+vendor+" status|result to follow up")
	rubric := fs.String("rubric", "", "grade the review against this rubric file")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	scope, err := vendoragent.ParseScope(*uncommitted, *base, *commit, *pr)
	if err != nil {
		return fail("%s review: %v", vendor, err)
	}
	cwd, _ := os.Getwd()
	diff, err := vendoragent.Diff(context.Background(), cwd, scope)
	if err != nil {
		return fail("%s review: %v", vendor, err)
	}
	focus := strings.Join(fs.Args(), " ")
	askArgs, cleanup, err := vendoragent.ReviewArgs(vendor, scope, diff, *adversarial, *background, focus, *rubric)
	if err != nil {
		return fail("%s review: %v", vendor, err)
	}
	defer cleanup()
	return cmdAsk(askArgs)
}

// cmdVendorExplore is `claude-code <v> explore [--no-web] [--background] [--format f] <question>`.
func cmdVendorExplore(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "explore")
	noWeb := fs.Bool("no-web", false, "skip the web-research leg; code scan only")
	background := fs.Bool("background", false, "detach; claude-code "+vendor+" status|result to follow up")
	format := fs.String("format", "text", "stream text or print the JSON result envelope")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	question := strings.Join(fs.Args(), " ")
	if question == "" {
		return fail("%s explore: usage: claude-code %s explore [--no-web] [--background] <question>", vendor, vendor)
	}
	cwd, _ := os.Getwd()
	askArgs := vendoragent.ExploreArgs(vendor, cwd, question, !*noWeb, *background)
	if *format != "text" {
		task := askArgs[len(askArgs)-1]
		askArgs = append(askArgs[:len(askArgs)-1], "--format", *format, task)
	}
	return cmdAsk(askArgs)
}

// cmdVendorTransfer is `claude-code <v> transfer [--source f] [--wait] [note…]`.
func cmdVendorTransfer(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "transfer")
	source := fs.String("source", "", "transcript .jsonl (default: newest session for this repo)")
	wait := fs.Bool("wait", false, "block for the answer instead of detaching")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	cwd, _ := os.Getwd()
	src := *source
	if src != "" {
		if err := vendoragent.ValidateSource(src); err != nil {
			return fail("%s transfer: %v", vendor, err)
		}
	} else {
		var err error
		src, err = vendoragent.DefaultSource(cwd)
		if err != nil {
			return fail("%s transfer: %v", vendor, err)
		}
	}
	fmt.Fprintf(os.Stderr, "transfer: source %s\n", src)
	note := strings.Join(fs.Args(), " ")
	prompt, err := vendoragent.Handoff(src, cwd, note)
	if err != nil {
		return fail("%s transfer: %v", vendor, err)
	}
	code := cmdAsk(vendoragent.TransferArgs(vendor, cwd, prompt, *wait))
	if code == 0 && *wait {
		if rec, _, err := vendoragent.Result(context.Background(), vendor, cwd, ""); err == nil {
			if hint := resumeHint(vendor, rec.SessionID); hint != "" {
				fmt.Printf("resume: %s\n", hint)
			}
		}
	}
	return code
}

// cmdVendorModels is `claude-code <v> models`: the vendor's own catalog, or a note that it has none.
func cmdVendorModels(vendor string) int {
	a, ok := vendoragent.Lookup(vendor)
	if !ok {
		return fail("unknown vendor %q", vendor)
	}
	switch {
	case a.ModelsProvider != "":
		return cmdModels([]string{"--live", "--provider", a.ModelsProvider})
	case len(a.ModelsArgv) > 0:
		return launchVendor(vendor, a.ModelsArgv)
	default:
		fmt.Printf("%s: no model catalog\n", vendor)
		return 0
	}
}

// vendorLaunchFlags lists the harness flags a bare launch consumes (launch.ParseArgs) for the help page.
func vendorLaunchFlags(vendor string) *flag.FlagSet {
	fs := helptext.NewFlagSet(vendor, helptext.Page{Usage: "claude-code " + vendor}) // display only; ParseArgs parses
	fs.String("model", "", "model for this launch, [provider:]id")
	fs.String("provider", "", "provider row for this launch")
	fs.Bool("direct", false, "skip the router; hand the CLI the provider route")
	fs.Bool("print-env", false, "print the resolved binary, argv and env; do not launch")
	if vendor == "codex" {
		fs.String("f", "", "config.toml whose keys ride as -c overrides")
	}
	return fs
}

// vendorBinary is the wrapped CLI a vendor verb drives (gemini → agy).
func vendorBinary(vendor string) string {
	spec, _ := vendors.Lookup(vendor)
	return spec.Binary
}
