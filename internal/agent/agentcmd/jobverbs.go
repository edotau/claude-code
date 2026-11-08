package agentcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/shapes"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func agentsCancel(rec job.Record, format, cwd string) int {
	rec, err := job.CancelJob(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return helptext.Fail("agents cancel: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}{rec.ID, rec.Status, rec.Error}); err != nil {
			return helptext.Fail("agents cancel: %v", err)
		}
		return 0
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// agentsJobPage returns `agents <verb> -h`.
func agentsJobPage(verb string) helptext.Page {
	about := map[string]string{
		"status": "Show one dispatch's state, whichever agent ran it (claude included).",
		"result": "Print a finished dispatch's output; --format json prints its envelope.",
		"tail":   "Print a dispatch's log; --format json streams its events.jsonl instead.",
		"cancel": "Interrupt a running background dispatch, whichever agent runs it.",
	}[verb]
	return helptext.Page{Usage: "claude-code agents " + verb + " <id> [flags]", About: about}
}

func agentsResult(rec job.Record, format, cwd string) int {
	rec, log, err := job.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, client.RecordEnvelope(rec, os.Stderr)); err != nil {
			return helptext.Fail("agents result: %v", err)
		}
		return ResultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if line := client.RecordEnvelope(rec, os.Stderr).RanOn; line != "" {
		fmt.Println("ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		fmt.Printf("resume: claude-code agents ask --agent %s --session %s\n", rec.Agent, rec.SessionID)
	}
	return ResultExit(rec)
}

// agentsSendPage is `agents send -h`.
var agentsSendPage = helptext.Page{
	Usage: "claude-code agents send <id> <task | -> [flags]",
	About: "Send one user turn to a live claude or gemini session (ask --agent <a> --live) and stream " +
		"that turn's events; exits on its result. One turn at a time per session.",
}

func agentsStatus(rec job.Record, format, cwd string) int {
	rows, err := job.Status(context.Background(), rec.Agent, cwd, rec.ID, true)
	if err != nil {
		return helptext.Fail("agents status: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rows); err != nil {
			return helptext.Fail("agents status: %v", err)
		}
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, StatusDetail(r))
	}
	return 0
}

func agentsTail(rec job.Record, format string, follow bool, cwd string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if format == helptext.JSON {
		rec, err = job.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = job.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
	}
	if err != nil {
		return helptext.Fail("agents tail: %v", err)
	}
	return TailExit(rec, follow)
}

// Job is `claude-code agents status|result|tail|cancel <id>`: the agent comes from the dispatch record.
func Job(verb string, args []string) int {
	fs := helptext.NewFlagSet("agents "+verb, agentsJobPage(verb))
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
	cwd, _ := os.Getwd()
	switch verb {
	case "status":
		return agentsStatus(rec, *format, cwd)
	case "result":
		return agentsResult(rec, *format, cwd)
	case "tail":
		return agentsTail(rec, *format, *follow, cwd)
	}
	return agentsCancel(rec, *format, cwd)
}

// Send is `claude-code agents send <id> <task>`.
func Send(args []string) int {
	fs := helptext.NewFlagSet("agents send", agentsSendPage)
	format := fs.String("format", helptext.Text, "stream text, print the JSON envelope, or stream-json NDJSON events")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) < 2 || !slices.Contains([]string{helptext.Text, helptext.JSON, client.FormatStreamJSON}, *format) {
		fs.Usage()
		return 2
	}
	prompt, err := client.ResolveAskPrompt(strings.Join(pos[1:], " "))
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	id := pos[0]
	if rec, ferr := job.Find(id); ferr == nil {
		id = rec.ID
	}
	env, err := job.SendLive(ctx, id, prompt, ask.StdoutSink(*format, id))
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	printLiveEnvelope(env, *format)
	if env.Status != wire.StatusOK {
		return 1
	}
	return 0
}

func printLiveEnvelope(env wire.Envelope, format string) {
	if format == helptext.JSON {
		body, _ := json.Marshal(env)
		fmt.Println(string(body))
	} else if format == helptext.Text && env.Answer != "" && !strings.HasSuffix(env.Answer, "\n") {
		fmt.Println()
	}
}

// ResultExit reports a failed job's error on stderr and returns 1; a finished ok job returns 0.
func ResultExit(rec job.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", shapes.Redact(rec.Error)) // the stderr tail in it may carry a key
	return 1
}

// StatusDetail is a status row's tail: progress and preview, or the preview plus the one-line error text.
func StatusDetail(r job.Row) string {
	d := r.Preview
	if r.Progress != "" {
		d = "[" + r.Progress + "] " + d
	}
	if r.Error != "" {
		d += " -- " + shapes.Preview(r.Error, 300)
	}
	return d
}

// TailExit is a tail's exit code: 1 for an errored job, or for a followed job that is still "running" (lost).
func TailExit(rec job.Record, follow bool) int {
	if rec.Status == "running" && follow {
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	return ResultExit(rec)
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
		return helptext.BadFormat(name+" status", err)
	}
	ref := firstPositional(pos)
	cwd, _ := os.Getwd()
	rows, err := job.Status(context.Background(), name, cwd, ref, *all)
	if err != nil {
		return helptext.Fail("%s status: %v", name, err)
	}
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail(name+" status", rows)
	}
	if len(rows) == 0 {
		fmt.Printf("no %s dispatches logged yet\n", name)
		return 0
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, StatusDetail(r))
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
	ref := firstPositional(pos)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, _ := os.Getwd()
	rec, err := job.Tail(ctx, name, cwd, ref, *follow, os.Stdout)
	if err != nil {
		return helptext.Fail("%s tail: %v", name, err)
	}
	return TailExit(rec, *follow)
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
		return helptext.BadFormat(name+" result", err)
	}
	ref := firstPositional(pos)
	cwd, _ := os.Getwd()
	rec, log, err := job.JobResult(context.Background(), name, cwd, ref)
	if err != nil {
		return helptext.Fail("%s result: %v", name, err)
	}
	s, _ := agent.LookupSchema(name)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			ID      string `json:"id"`
			Session string `json:"session,omitempty"`
			Resume  string `json:"resume,omitempty"`
			Log     string `json:"log"`
		}{rec.ID, rec.SessionID, s.ResumeHint(rec.SessionID), log}); err != nil {
			return helptext.Fail("%s result: %v", name, err)
		}
		return ResultExit(rec)
	}
	fmt.Print(log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Println()
	}
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		printResumeHint(s, rec.SessionID)
	}
	return ResultExit(rec)
}

// agentCancel is `claude-code <v> cancel [id]`.
func agentCancel(name string, args []string) int {
	fs := newAgentFlags(name, "cancel")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ref := fs.Arg(0)
	cwd, _ := os.Getwd()
	rec, err := job.CancelJob(context.Background(), name, cwd, ref)
	if err != nil {
		return helptext.Fail("%s cancel: %v", name, err)
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}
