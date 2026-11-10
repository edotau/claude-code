package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Status prints agent's jobs in this repo (all: every repo; ref narrows) as JSON or text; empty stands in for none.
func Status(prefix, agentName, ref string, all bool, format, empty string) int {
	cwd, _ := os.Getwd()
	rows, err := job.Status(context.Background(), agentName, cwd, ref, all)
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail(prefix, rows)
	}
	if len(rows) == 0 && empty != "" {
		fmt.Println(empty)
	}
	for _, r := range rows {
		fmt.Printf("%-24s %-8s %-10s %s\n", r.ID, r.Status, r.Elapsed, r.Detail())
	}
	return 0
}

// PrintResult prints a dispatch's log, its ask line and resume command, or with json its envelope.
func PrintResult(rec job.Record, format string) int {
	cwd, _ := os.Getwd()
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
	out := &ErrWriter{W: os.Stdout}
	fmt.Fprint(out, log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Fprintln(out)
	}
	if line := client.RecordEnvelope(rec, os.Stderr).RanOn; line != "" {
		fmt.Fprintln(out, "ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Fprintf(out, "session: %s\n", rec.SessionID)
		fmt.Fprintf(out, "resume: claude-code agents ask --agent %s --session %s\n", rec.Agent, rec.SessionID)
	}
	if out.Err != nil {
		return helptext.Fail("agents result: write output: %v", out.Err)
	}
	return ResultExit(rec)
}

// PrintAgentResult is `<agent> result`: a job's log, session and resume hint, or with json {id, session, resume, log}.
func PrintAgentResult(name, ref, format string) int {
	cwd, _ := os.Getwd()
	rec, log, err := job.JobResult(context.Background(), name, cwd, ref)
	if err != nil {
		return helptext.Fail("%s result: %v", name, err)
	}
	if format == helptext.JSON {
		s, _ := agent.LookupSchema(name)
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
		PrintResumeHint(name, rec.SessionID)
	}
	return ResultExit(rec)
}

// TailJob prints a job's log (json: its events); exit 1 for an errored job or a followed job that was lost.
func TailJob(prefix string, rec job.Record, format string, follow bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, _ := os.Getwd()
	var err error
	if format == helptext.JSON {
		rec, err = job.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = job.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
	}
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	if rec.Status == "running" && follow {
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	return ResultExit(rec)
}

// CancelJob interrupts agent's job ref and prints its id (json: id, status, error).
func CancelJob(prefix, agentName, ref, format string) int {
	cwd, _ := os.Getwd()
	rec, err := job.CancelJob(context.Background(), agentName, cwd, ref)
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail(prefix, struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}{rec.ID, rec.Status, rec.Error})
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// ResultExit reports a failed job's error on stderr and returns 1; a finished ok job returns 0.
func ResultExit(rec job.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", textutil.RedactSecrets(rec.Error)) // the stderr tail in it may carry a key
	return 1
}

// Send is `agents send`: one user turn to live session id, streamed in format; exit 1 unless the turn ends ok.
func Send(id, task, format string) int {
	prompt, err := client.ResolveAskPrompt(task)
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if rec, ferr := job.Find(id); ferr == nil {
		id = rec.ID
	}
	env, err := job.SendLive(ctx, id, prompt, StdoutSink(format, id))
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	if err := printLiveEnvelope(env, format); err != nil {
		return helptext.Fail("agents send: write output: %v", err)
	}
	if env.Status != wire.StatusOK {
		return 1
	}
	return 0
}

// printLiveEnvelope writes the turn's envelope (one JSON line) or closing newline (text), returning any write failure.
func printLiveEnvelope(env wire.Envelope, format string) error {
	if format == helptext.JSON {
		body, err := json.Marshal(env)
		if err != nil {
			return err
		}
		_, err = fmt.Println(string(body))
		return err
	}
	if format == helptext.Text && env.Answer != "" && !strings.HasSuffix(env.Answer, "\n") {
		_, err := fmt.Println()
		return err
	}
	return nil
}

// Setup is `<agent> setup`: check name's binary, login, route and projection; exit 1 when one fails.
func Setup(name, format string) int {
	row, ok := agent.LookupSchema(name)
	if !ok {
		row = agent.Schema{Name: name} // reported as an unknown agent, by name
	}
	rep := row.Setup()
	if format == helptext.JSON {
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
