package ask

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Status prints agent's jobs in this repo (all: every repo; ref narrows) as JSON or text; empty stands in for none.
func Status(prefix, agentName, ref string, all bool, format, empty string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	rows, err := agent.Status(context.Background(), agentName, cwd, ref, all)
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
func PrintResult(rec agent.Record, format string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	rec, log, err := agent.JobResult(context.Background(), rec.Agent, cwd, rec.ID)
	if err != nil {
		return helptext.Fail("agents result: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, rec.ResultEnvelope(os.Stderr)); err != nil {
			return helptext.Fail("agents result: %v", err)
		}
		return ResultExit(rec)
	}
	out := &textutil.ErrWriter{W: os.Stdout}
	printLog(out, log)
	if line := rec.ResultEnvelope(os.Stderr).RanOn; line != "" {
		fmt.Fprintln(out, "ask:", line)
	}
	if rec.SessionID != "" {
		fmt.Fprintf(out, "session: %s\n", rec.SessionID)
		fprintResumeHint(out, rec.Agent, rec.SessionID)
	}
	if out.Err != nil {
		return helptext.Fail("agents result: write output: %v", out.Err)
	}
	return ResultExit(rec)
}

// printLog writes a job log, adding a trailing newline when it lacks one.
func printLog(w io.Writer, log string) {
	fmt.Fprint(w, log)
	if log != "" && !strings.HasSuffix(log, "\n") {
		fmt.Fprintln(w)
	}
}

// PrintAgentResult is `<agent> result`: a job's log, session and resume hint, or with json {id, session, resume, log}.
func PrintAgentResult(name, ref, format string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s result: %v", name, err)
	}
	rec, log, err := agent.JobResult(context.Background(), name, cwd, ref)
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
	printLog(os.Stdout, log)
	if rec.SessionID != "" {
		fmt.Printf("session: %s\n", rec.SessionID)
		PrintResumeHint(name, rec.SessionID)
	}
	return ResultExit(rec)
}

// TailJob prints a job's log (json: its events); exit 1 for an errored job or a followed job that was lost.
func TailJob(prefix string, rec agent.Record, format string, follow bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	if format == helptext.JSON {
		rec, err = agent.TailEvents(ctx, rec, follow, os.Stdout)
	} else {
		rec, err = agent.Tail(ctx, rec.Agent, cwd, rec.ID, follow, os.Stdout)
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
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	rec, err := agent.CancelJob(context.Background(), agentName, cwd, ref)
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
func ResultExit(rec agent.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", textutil.RedactSecrets(rec.Error)) // the stderr tail in it may carry a key
	return 1
}

// Send is `agents send`: one user turn to live session id, streamed in format; exit 1 unless the turn ends ok.
func Send(id, task, format string) int {
	prompt, err := ResolveAskPrompt(task)
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if rec, ferr := agent.Find(id); ferr == nil {
		id = rec.ID
	}
	env, err := agent.SendLive(ctx, id, prompt, StdoutSink(format, id))
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

// liveExclusive are the ask flags a live session cannot honor: it has no single task, route or envelope of its own.
var liveExclusive = []string{"background", "host", "batch", "retry", "job", "env", "attach", "no-tools", "output-schema",
	"rubric", "log", "stdin-framed", "fallback", "native"}

// askLiveCheck rejects --live with another agent, a task, or a flag that has no meaning for a held-open session.
func askLiveCheck(fs *flag.FlagSet, name string) error {
	if row, _ := agent.LookupSchema(name); !row.HasLive() {
		return fmt.Errorf("--live is claude or gemini only, not %s", name)
	}
	if fs.NArg() > 0 {
		return errors.New("--live takes no task: start it, then claude-code agents send <id> <task>")
	}
	for _, f := range liveExclusive {
		if helptext.Explicit(fs, f) {
			return fmt.Errorf("--live cannot be combined with --%s", f)
		}
	}
	return nil
}

// liveRequest is the RunOptions a live holder keeps for its whole session.
func liveRequest(s *askState, perms agent.Permissions, name string) agent.RunOptions {
	return agent.RunOptions{Model: *s.Model, Provider: *s.Provider, Effort: *s.Effort, WorkDir: *s.Workdir, SessionID: *s.Session,
		Tools: *s.Tools, ReadOnly: *s.ReadOnly, Permissions: perms, Timeout: *s.Timeout, KeepEnv: *s.KeepEnv, NoPersist: *s.NoPersist, SafeMode: *s.SafeMode,
		DebugFile: absDebugFile(*s.DebugFile), SystemPrompt: *s.SystemPrompt, AppendSystemPrompt: *s.AppendSystemPrompt,
		Autonomous: *s.Autonomous, Direct: *s.Direct, Lead: *s.lead, Agent: name, Background: true}
}

func startLive(s *askState, perms agent.Permissions, name, as, format string) int {
	r, err := agent.Lookup(name)
	if err == nil {
		err = r.Available()
	}
	if err == nil {
		err = agent.CheckLiveCap()
	}
	if err != nil {
		return failAsk(format, name, err)
	}
	req := liveRequest(s, perms, name)
	req.Identity = as
	rec, err := NewClient().Record(context.Background(), req, nil)
	if err != nil {
		return failAsk(format, name, err)
	}
	rec.Live = true
	if err := rec.Save(); err != nil {
		return failAsk(format, name, err)
	}
	if err := agent.Spawn(&rec, nil); err != nil {
		return failAsk(format, name, abandonLive(&rec, err))
	}
	if err := agent.WaitLiveReady(rec.ID, rec.PID); err != nil {
		return failAsk(format, name, abandonLive(&rec, fmt.Errorf("live session %s did not start: %w (log: %s)", rec.ID, err, rec.Log)))
	}
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			ID     string `json:"id"`
			Socket string `json:"socket"`
			Log    string `json:"log"`
		}{rec.ID, agent.LiveSocket(rec.ID), rec.Log})
		fmt.Println(string(body))
		return 0
	}
	fmt.Printf("live session %s — claude-code agents send %s \"<task>\" · agents cancel %s · idle limit %s\n", rec.ID, rec.ID, rec.ID, agent.IdleOf(req))
	return 0
}

// abandonLive stops a holder that never became ready and marks its record failed; an unstoppable holder is not marked.
func abandonLive(rec *agent.Record, cause error) error {
	if rec.PID > 0 {
		if err := agent.Cancel(rec); err != nil {
			return fmt.Errorf("%w; holder pid %d still running, stop failed: %w", cause, rec.PID, err)
		}
	}
	if rec.Status == "running" || rec.Status == "" {
		rec.Status, rec.Error = "error", cause.Error()
		_ = rec.Save()
	}
	return cause
}
