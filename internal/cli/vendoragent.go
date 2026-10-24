package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/vendoragent"
)

// vendorSubverbs are the words vendorVerb routes to cmdVendorSub.
var vendorSubverbs = map[string]bool{
	"resume": true, "status": true, "result": true, "cancel": true, "setup": true, "models": true,
	"review": true, "transfer": true, "explore": true,
}

// vendorUsageWidth is the description column: the longest label ("  claude-code opencode [launch flags] [args…]") plus a gap.
const vendorUsageWidth = 46

// vendorUsage renders the frozen help block (plans/2026-10-24-vendor-agent-verbs/contract.md) for one vendor.
func vendorUsage(vendor string) string {
	rows := [][2]string{
		{vendor + " [launch flags] [args…]", "launch " + vendor + "; args forwarded"},
		{vendor + " -- <args…>", "force passthrough to " + vendor},
		{vendor + " ask [flags] <task>", "one-shot run (= ask --agent " + vendor + ")"},
		{vendor + " resume <id> <task>", "continue a session"},
		{vendor + " review [scope] [focus]", "diff review; --adversarial"},
		{vendor + " explore <question>", "scan code + web research → brief"},
		{vendor + " transfer [--source f]", "hand this Claude session to " + vendor},
		{vendor + " status [id] [--all]", "background jobs in this repo"},
		{vendor + " result [id]", "a finished job's answer + session"},
		{vendor + " cancel [id]", "interrupt a running job"},
		{vendor + " setup [--format json]", "binary, login, route, projection"},
		{vendor + " models", "the vendor's own model catalog"},
	}
	if a, ok := vendoragent.Lookup(vendor); ok {
		rows = append(rows, a.ExtraUsage...)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "claude-code %s — the %s agent\n\nUSAGE\n", vendor, vendor)
	for _, r := range rows {
		for _, l := range helptext.WrapRow("  claude-code "+r[0], r[1], vendorUsageWidth, 80) {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// newVendorFlags makes a ContinueOnError FlagSet named "<vendor> <verb>" with the shared usage printer.
func newVendorFlags(vendor, verb string) *flag.FlagSet {
	fs := flag.NewFlagSet(vendor+" "+verb, flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	return fs
}

// cmdVendorSub dispatches a vendor's background-job and setup subverbs (resume/status/result/cancel/setup/models).
func cmdVendorSub(vendor string, args []string) int {
	switch args[0] {
	case "resume":
		return cmdVendorResume(vendor, args[1:])
	case "status":
		return cmdVendorStatus(vendor, args[1:])
	case "result":
		return cmdVendorResult(vendor, args[1:])
	case "cancel":
		return cmdVendorCancel(vendor, args[1:])
	case "setup":
		return cmdVendorSetup(vendor, args[1:])
	case "models":
		return cmdVendorModels(vendor)
	case "review":
		return cmdVendorReview(vendor, args[1:])
	case "explore":
		return cmdVendorExplore(vendor, args[1:])
	case "transfer":
		return cmdVendorTransfer(vendor, args[1:])
	}
	return fail("%s: unknown subcommand %q", vendor, args[0])
}

// cmdVendorResume is `claude-code <v> resume <id> <task…>` = `ask --agent <v> --session <id> <task…>`.
func cmdVendorResume(vendor string, args []string) int {
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
		return 2
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
		_ = outfmt.Write(os.Stdout, rows)
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

// cmdVendorResult is `claude-code <v> result [id] [--format text|json]`.
func cmdVendorResult(vendor string, args []string) int {
	fs := newVendorFlags(vendor, "result")
	format := outfmt.Flag(fs)
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return 2
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
		_ = outfmt.Write(os.Stdout, struct {
			ID      string `json:"id"`
			Session string `json:"session,omitempty"`
			Resume  string `json:"resume,omitempty"`
			Log     string `json:"log"`
		}{rec.ID, rec.SessionID, resumeHint(vendor, rec.SessionID), log})
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
	ref := ""
	if len(args) > 0 {
		ref = args[0]
	}
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%s setup: %v", vendor, err)
	}
	rep := vendoragent.Setup(vendor)
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, rep)
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
	if err := fs.Parse(args); err != nil {
		return 2
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
	if err := fs.Parse(args); err != nil {
		return 2
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
	if err := fs.Parse(args); err != nil {
		return 2
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
