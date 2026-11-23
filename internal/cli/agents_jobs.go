package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const waitProgressWidth = 80

// waitProgressEvery matches the record's 2 s activity refresh; waitLoad and waitSlice are test seams.
var (
	waitProgressEvery = 2 * time.Second
	waitLoad          = agent.LoadRecord
	waitSlice         = agent.Wait
)

// waitRows is agent.Wait; with progress it waits in slices and prints a stderr line per activity change.
func waitRows(ctx context.Context, ids []string, timeout time.Duration, progress bool) ([]agent.WaitResult, error) {
	if !progress {
		return agent.Wait(ctx, ids, timeout, time.Second)
	}
	return waitRowsProgress(ctx, ids, timeout, os.Stderr)
}

func waitRowsProgress(ctx context.Context, ids []string, timeout time.Duration, out io.Writer) ([]agent.WaitResult, error) {
	deadline := time.Now().Add(timeout)
	last := make(map[string]string, len(ids))
	for {
		remaining := time.Until(deadline)
		rows, err := waitSlice(ctx, ids, min(max(remaining, 0), waitProgressEvery), time.Second)
		if err != nil {
			return nil, err
		}
		if !waitHasTimeout(rows) || time.Until(deadline) <= 0 {
			return rows, nil
		}
		for _, row := range rows {
			if row.State != agent.WaitTimeout {
				continue
			}
			rec, err := waitLoad(row.ID)
			if err != nil {
				continue
			}
			line := waitProgressLine(rec, time.Now())
			if line == last[row.ID] {
				continue
			}
			if _, err := fmt.Fprintln(out, line); err != nil {
				return nil, err
			}
			last[row.ID] = line
		}
	}
}

func waitHasTimeout(rows []agent.WaitResult) bool {
	for _, row := range rows {
		if row.State == agent.WaitTimeout {
			return true
		}
	}
	return false
}

// waitProgressLine is one --progress row: id, step, tools, tokens so far, last log text; it is also the change key.
func waitProgressLine(rec agent.Record, now time.Time) string {
	p := agent.ProgressOf(rec, now)
	tools, usage := int64(p.Tools), wire.Usage{}
	if rec.Activity != nil {
		tools, usage = rec.Activity.Tools, rec.Activity.Usage
	}
	parts := []string{rec.ID, rec.Progress}
	if tools >= 0 {
		parts = append(parts, fmt.Sprintf("%d tools", tools))
	}
	parts = append(parts, "↑"+tokenOrDash(usage.InputTokens)+" ↓"+tokenOrDash(usage.OutputTokens), p.Doing)
	parts = slices.DeleteFunc(parts, func(s string) bool { return s == "" })
	return textutil.TruncateCells(strings.Join(parts, " · "), waitProgressWidth, "…")
}

// tokenOrDash shows "-" for 0: agents report usage only in their final event, so a running job usually has none.
func tokenOrDash(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

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
		return agentsDispatchLog(*format)
	}
	rec, err := agent.LoadRecord(pos[0])
	if err != nil {
		return helptext.Fail("agents status: %v", err)
	}
	return ask.Status("agents status", rec.Agent, rec.ID, true, *format, "")
}

func agentsDispatchLog(format string) int {
	if format != helptext.JSON {
		return ask.DispatchLog("agents status", format)
	}
	recs := agent.RecentRecords(20)
	for i, r := range recs {
		if r.Alive() {
			if line := r.QueueLine(time.Now()); line != "" {
				recs[i].Status, recs[i].Progress = "queued", line
			}
		} else if strings.HasPrefix(r.Progress, "queued ") {
			recs[i].Progress = ""
		}
	}
	return helptext.WriteJSONOrFail("agents status", recs)
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
	after          int64
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
	case o.after < 0:
		return errors.New("--after needs a non-negative sequence")
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
	fs.Int64Var(&o.after, "after", 0, "with --events, skip sequences at or below N")
	fs.StringVar(&o.save, "save", "", "write the result text to this file and print one summary line")
	fs.IntVar(&o.head, "head", 0, "with --save, also print the first N lines of the saved text")
	fs.BoolVar(&o.verdict, "verdict", false, "print one status line and the last --lines non-empty answer lines")
	fs.IntVar(&o.lines, "lines", 10, "with --verdict, how many answer lines to print")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	afterSet := false
	fs.Visit(func(f *flag.Flag) { afterSet = afterSet || f.Name == "after" })
	if afterSet && !o.events {
		fs.Usage()
		return helptext.FailUsage("agents result: --after needs --events")
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
			Follow: o.follow, Events: o.events, After: o.after})
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
