package cli

import (
	"errors"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdAgentsStatus is `agents status [<id>] [--agent A] [--all]`: recent dispatches, one agent's jobs, or one dispatch.
func cmdAgentsStatus(args []string) int {
	fs := agentsFlags("status")
	agentName := fs.String("agent", "", "list this agent's jobs in this repo (with an <id>, narrow to it)")
	all := fs.Bool("all", false, "with --agent, every repo, not just this one")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents status", err)
	}
	if len(pos) > 1 || (*all && *agentName == "") {
		fs.Usage()
		return 2
	}
	switch {
	case *agentName != "":
		empty := ""
		if len(pos) == 0 {
			empty = "no " + *agentName + " dispatches logged yet"
		}
		return ask.Status("agents status", *agentName, firstPositional(pos), *all, *format, empty)
	case len(pos) == 0:
		return ask.DispatchLog("agents status", *format)
	}
	rec, err := agent.LoadRecord(pos[0])
	if err != nil {
		return helptext.Fail("agents status: %v", err)
	}
	return ask.Status("agents status", rec.Agent, rec.ID, true, *format, "")
}

// resultFlags are the `agents result` flags.
type resultFlags struct {
	agent          string
	follow, events bool
	save           string
	head           int
	verdict        bool
	lines          int
	format         string
}

// check reports why the flag combination is refused, given whether an <id> was named.
func (o resultFlags) check(hasID bool) error {
	if err := o.checkCounts(); err != nil {
		return err
	}
	if err := o.checkSink(hasID); err != nil {
		return err
	}
	switch {
	case o.events && o.format == helptext.JSON:
		return errors.New("--events has no JSON form; drop --format json")
	case !hasID && o.agent == "":
		return errors.New("name a dispatch <id>, or --agent for that agent's newest job here")
	}
	return nil
}

// checkCounts reports why --head or --verdict is refused with --save or --lines.
func (o resultFlags) checkCounts() error {
	switch {
	case o.head < 0 || (o.head > 0 && o.save == ""):
		return errors.New("--head needs --save and a positive count")
	case o.verdict && (o.save != "" || o.lines < 1):
		return errors.New("--verdict excludes --save and needs --lines >= 1")
	}
	return nil
}

// checkSink reports why --save or --verdict is refused with -f, --events or no <id>.
func (o resultFlags) checkSink(hasID bool) error {
	sink := o.save != "" || o.verdict
	switch {
	case sink && (o.follow || o.events):
		return errors.New("-f and --events cannot combine with --save or --verdict")
	case sink && !hasID:
		return errors.New("--save and --verdict need a dispatch <id>")
	}
	return nil
}

// cmdAgentsResult is `agents result [<id>] [--agent A] [-f] [--events] [--save F] [--verdict]`.
func cmdAgentsResult(args []string) int {
	fs := agentsFlags("result")
	var o resultFlags
	fs.StringVar(&o.agent, "agent", "", "with no <id>, that agent's newest job in this repo")
	fs.BoolVar(&o.follow, "f", false, "follow a running job until it finishes, with a heartbeat on silence")
	fs.BoolVar(&o.events, "events", false, "stream the job's events.jsonl instead of its log")
	fs.StringVar(&o.save, "save", "", "write the result text to this file and print one summary line")
	fs.IntVar(&o.head, "head", 0, "with --save, also print the first N lines of the saved text")
	fs.BoolVar(&o.verdict, "verdict", false, "print one status line and the last --lines non-empty answer lines")
	fs.IntVar(&o.lines, "lines", 10, "with --verdict, how many answer lines to print")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents result", err)
	}
	o.format = *format
	if len(pos) > 1 {
		fs.Usage()
		return 2
	}
	if err := o.check(len(pos) == 1); err != nil {
		return helptext.FailUsage("agents result: %v", err)
	}
	return runAgentsResult(o, firstPositional(pos))
}

// runAgentsResult runs --save and --verdict on the dispatch record, and every other form through ask.Result.
func runAgentsResult(o resultFlags, id string) int {
	if o.save == "" && !o.verdict {
		return ask.Result(ask.ResultOptions{Prefix: "agents result", Agent: o.agent, Ref: id, Format: o.format,
			Follow: o.follow, Events: o.events})
	}
	rec, err := agent.LoadRecord(id)
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	if o.save != "" {
		return ask.SaveResult(rec, o.save, o.head, o.format)
	}
	return ask.Verdict(rec, o.lines, o.format)
}

// cancelFlags are the `agents cancel` flags.
type cancelFlags struct {
	agent        string
	lost, dryRun bool
	format       string
}

// check reports why the flag combination is refused, given how many positionals were named.
func (o cancelFlags) check(nPos int) error {
	switch {
	case o.dryRun && !o.lost:
		return errors.New("--dry-run needs --lost")
	case o.lost && (nPos > 0 || o.agent != "" || o.format == helptext.JSON):
		return errors.New("--lost takes no <id>, --agent or --format json")
	}
	return nil
}

// cmdAgentsCancel is `agents cancel (<id> | --lost [--dry-run]) [--agent A]`.
func cmdAgentsCancel(args []string) int {
	fs := agentsFlags("cancel")
	var o cancelFlags
	fs.StringVar(&o.agent, "agent", "", "with no <id>, that agent's single running job in this repo")
	fs.BoolVar(&o.lost, "lost", false, "mark dead-but-running dispatch records as error; drop stale markers")
	fs.BoolVar(&o.dryRun, "dry-run", false, "with --lost, print what would change and write nothing")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents cancel", err)
	}
	o.format = *format
	if len(pos) > 1 || (len(pos) == 0 && o.agent == "" && !o.lost) {
		fs.Usage()
		return 2
	}
	if err := o.check(len(pos)); err != nil {
		return helptext.FailUsage("agents cancel: %v", err)
	}
	if o.lost {
		return tree.Reap(o.dryRun, os.Stdout, os.Stderr)
	}
	return cancelJob(o.agent, firstPositional(pos), o.format)
}

// cancelJob cancels one dispatch; with no agent named, the dispatch record names it.
func cancelJob(name, ref, format string) int {
	if name == "" {
		rec, err := agent.LoadRecord(ref)
		if err != nil {
			return helptext.Fail("agents cancel: %v", err)
		}
		name, ref = rec.Agent, rec.ID
	}
	return ask.CancelJob("agents cancel", name, ref, format)
}
