package ask

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
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

// ResultOptions select `agents result`: Ref "" needs Agent; Follow streams until the job ends, Events the event file.
type ResultOptions struct {
	Prefix, Agent, Ref, Format string
	Follow, Events             bool
}

// Result prints a job's log, ask line and resume command (json: its envelope); exit 1 for an errored or lost job.
func Result(o ResultOptions) int {
	prefix := cmp.Or(o.Prefix, "agents result")
	if o.Agent == "" && o.Ref == "" {
		return helptext.FailUsage("%s: an id or --agent is required", prefix)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	name, ref := o.Agent, o.Ref
	if name == "" { // a bare id names its agent in its record
		rec, err := agent.LoadRecord(ref)
		if err != nil {
			return helptext.Fail("%s: %v", prefix, err)
		}
		name, ref = rec.Agent, rec.ID
	}
	if o.Follow || o.Events {
		return streamResult(prefix, o, name, cwd, ref)
	}
	rec, log, err := agent.JobResult(context.Background(), name, cwd, ref)
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	return emitResult(prefix, rec, log, o.Format)
}

// streamResult follows (or just copies) the job's log or events; a followed log's trailer closes it, without the log.
func streamResult(prefix string, o ResultOptions, name, cwd, ref string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rec, err := agent.ResolveJob(ctx, name, cwd, ref)
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	out := &endWriter{w: os.Stdout}
	switch {
	case o.Events:
		rec, err = agent.TailEvents(ctx, rec, o.Follow, os.Stdout)
	case o.Format == helptext.JSON: // the envelope is the only JSON: wait for the job, stream nothing
		rec, err = agent.Tail(ctx, name, cwd, rec.ID, true, io.Discard)
	default:
		rec, err = agent.Tail(ctx, name, cwd, rec.ID, true, out)
	}
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	if rec.Status == "running" && o.Follow {
		fmt.Fprintf(os.Stderr, "error: job %s is lost (process gone)\n", rec.ID)
		return 1
	}
	if o.Events {
		return resultExit(rec)
	}
	if out.last != 0 && out.last != '\n' {
		fmt.Println()
	}
	return emitResult(prefix, rec, "", o.Format)
}

// endWriter remembers the last byte written, so a trailer can start on a fresh line.
type endWriter struct {
	w    io.Writer
	last byte
}

func (e *endWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		e.last = p[len(p)-1]
	}
	return e.w.Write(p)
}

// emitResult prints rec's envelope (json) or log, ask line and resume hint (text), and maps the job to an exit code.
func emitResult(prefix string, rec agent.Record, log, format string) int {
	if format == helptext.JSON {
		env := struct {
			wire.Envelope
			Observed *agent.Observed `json:"observed,omitempty"`
		}{maskEnvelope(rec.ResultEnvelope(os.Stderr)), agent.ObservedOf(rec)}
		if err := helptext.WriteJSON(os.Stdout, env); err != nil {
			return helptext.Fail("%s: %v", prefix, err)
		}
		return resultExit(rec)
	}
	if _, err := os.Stdout.WriteString(redact.Text(resultText(rec, log))); err != nil {
		return helptext.Fail("%s: write output: %v", prefix, err)
	}
	return resultExit(rec)
}

var cancelDispatch = agent.CancelJob

// CancelJob interrupts agent's job ref and prints its id (json: id, status, error).
func CancelJob(prefix, agentName, ref, format string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("%s: %v", prefix, err)
	}
	rec, err := cancelDispatch(context.Background(), agentName, cwd, ref)
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
	if rec.AlreadyFinished {
		fmt.Printf("%s already finished: %s\n", rec.ID, rec.Status)
		return 0
	}
	fmt.Printf("cancelled %s\n", rec.ID)
	return 0
}

// resultExit reports a failed job's error on stderr and returns 1; a finished ok job returns 0.
func resultExit(rec agent.Record) int {
	if rec.Status != "error" {
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: %s\n", redact.Text(textutil.RedactSecrets(rec.Error))) // the stderr tail in it may carry a key
	return 1
}

// ParentTarget is the `agents send` target that addresses the calling leaf's dispatcher instead of a session.
const ParentTarget = "parent"

// sendParent files task, literally, as a note in the dispatcher's inbox; with none (or a full one) it says so, exit 0.
func sendParent(task string) int {
	sent, err := worktree.NoteDispatcher(task)
	switch {
	case errors.Is(err, worktree.ErrNoteLimit):
		fmt.Fprintf(os.Stderr, "agents send: %v\n", err)
	case err != nil:
		return helptext.Fail("agents send: %v", err)
	case !sent:
		fmt.Fprintln(os.Stderr, "agents send: no dispatcher to notify")
	}
	return 0
}

// Send is `agents send`: a note to the dispatcher (id "parent"), a live turn, or a resume; exit 1 unless ok.
func Send(id, task, format string) int {
	if id == ParentTarget {
		return sendParent(task)
	}
	prompt, err := resolveAskPrompt(task)
	if err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if rec, ferr := agent.LoadRecord(id); ferr == nil {
		id = rec.ID
		if !rec.Live {
			return resumeSend(ctx, rec, prompt, format)
		}
	}
	env, err := agent.SendLive(ctx, id, prompt, stdoutSink(format, id))
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

// resumeSend answers a finished dispatch's session through the --retry path; the new dispatch is rec's child.
func resumeSend(ctx context.Context, rec agent.Record, prompt, format string) int {
	if err := checkResumable(rec); err != nil {
		return helptext.Fail("agents send: %v", err)
	}
	_, values := agent.RunValues(ctx, nil)
	values[proc.DispatchIDEnv], values[proc.DispatchDepthEnv] = rec.ID, strconv.Itoa(rec.Depth)
	return runCtx(proc.WithValues(ctx, values), []string{"--retry", rec.ID, "--format", format, "--", prompt}, "", os.Stdout)
}

// checkResumable names why rec cannot be resumed: still running, no session id recorded, or a fallback agent's session.
func checkResumable(rec agent.Record) error {
	switch {
	case rec.Alive():
		return fmt.Errorf("dispatch %s is still running and is not a live session (claude-code agents wait %s, then send)", rec.ID, rec.ID)
	case rec.SessionID == "":
		return fmt.Errorf("dispatch %s recorded no session id, so there is nothing to resume", rec.ID)
	case rec.RanAs != "" && rec.RanAs != rec.Agent:
		return fmt.Errorf("dispatch %s ran on %s as a fallback for %s; that session cannot be resumed as %s", rec.ID, rec.RanAs, rec.Agent, rec.Agent)
	}
	return nil
}

// printLiveEnvelope writes the turn's envelope (one JSON line) or closing newline (text), returning any write failure.
func printLiveEnvelope(env wire.Envelope, format string) error {
	if format == helptext.JSON {
		return writeEnvelope(os.Stdout, env)
	}
	if format == helptext.Text && env.Answer != "" && !strings.HasSuffix(env.Answer, "\n") {
		_, err := fmt.Println()
		return err
	}
	return nil
}

// liveExclusive are the ask flags a live session cannot honor: it has no single task, route or envelope of its own.
var liveExclusive = []string{"background", "host", "batch", "retry", "job", "env", "attach", "no-tools", "output-schema",
	"rubric", "stdin-framed", "fallback", "native"}

// askLiveCheck rejects --live with a persona, another agent, a task, or a flag a held-open session cannot honor.
func askLiveCheck(fs *flag.FlagSet, name string, hasPersona bool) error {
	if hasPersona {
		return errors.New("--live cannot run a persona (agents/<name>.md or an --agents key): use a plain vendor")
	}
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
func liveRequest(ctx context.Context, s *askState, perms agent.Permissions, name string) agent.RunOptions {
	_, cprof := router.ConfinedContext(ctx) // a live holder runs one hop with no chain: it starts on the confinement's profile
	return agent.RunOptions{Model: *s.Model, Provider: cmp.Or(router.ArmProfileContext(ctx), cprof, *s.Provider), Effort: *s.Effort, WorkDir: *s.Workdir, SessionID: *s.Session,
		Tools: *s.Tools, NoTools: *s.NoTools, Fleet: *s.Fleet, EnableMemory: *s.EnableMemory, ReadOnly: *s.ReadOnly, Permissions: perms, Timeout: *s.Timeout, KeepEnv: *s.KeepEnv, NoPersist: *s.NoPersist, SafeMode: *s.SafeMode,
		DebugFile: absDebugFile(*s.DebugFile), SystemPrompt: *s.SystemPrompt, AppendSystemPrompt: *s.AppendSystemPrompt,
		Autonomous: *s.Autonomous, Direct: *s.Direct, Lead: *s.lead, Agent: name, Background: true}
}

func startLive(ctx context.Context, s *askState, perms agent.Permissions, name, as, format string) int {
	req := liveRequest(ctx, s, perms, name)
	req.Identity = as
	if err := agent.CheckCapabilities(name, req); err != nil {
		return failAskCode(format, name, err, 2)
	}
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
	rec, err := newClient().Record(ctx, req, nil)
	if err != nil {
		return failAsk(format, name, err)
	}
	rec.Live = true
	if err := rec.Save(); err != nil {
		return failAsk(format, name, err)
	}
	if err := agent.SpawnContext(ctx, &rec, nil); err != nil {
		return failAsk(format, name, abandonLive(&rec, err))
	}
	if err := agent.WaitLiveReady(rec.ID, rec.PID); err != nil {
		return failAsk(format, name, abandonLive(&rec, fmt.Errorf("live session %s did not start: %w (log: %s)", rec.ID, err, rec.Log)))
	}
	return printLiveStarted(name, format, rec, req)
}

// printLiveStarted reports a ready live holder as one JSON line or the follow-up commands.
func printLiveStarted(name, format string, rec agent.Record, req agent.RunOptions) int {
	if format == helptext.JSON {
		if err := agent.WriteJSONLine(os.Stdout, struct {
			ID     string `json:"id"`
			Socket string `json:"socket"`
			Log    string `json:"log"`
		}{rec.ID, agent.LiveSocket(rec.ID), rec.Log}); err != nil {
			return failAsk(format, name, err)
		}
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
	// Cancel's own marks are replaced so the record says why the holder was abandoned.
	byCancel := rec.Status == "error" &&
		(rec.Error == "cancelled by claude-code agents cancel" || rec.Error == "process gone")
	if rec.Status == "running" || rec.Status == "" || byCancel {
		rec.Status, rec.Error = "error", cause.Error()
		_ = rec.Save()
	}
	return cause
}

// DispatchLog is bare `agents status`: the 20 newest dispatches, prompt preview scrubbed of credential shapes.
func DispatchLog(prefix, format string) int {
	recs := agent.RecentRecords(20)
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail(prefix, recs)
	}
	if len(recs) == 0 {
		fmt.Println("no dispatches logged yet")
		return 0
	}
	for _, r := range recs {
		fmt.Print(r.LogLine())
	}
	return 0
}
