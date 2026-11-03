package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/batch"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askPage is `ask -h`; the task is the trailing args, or stdin when absent or "-".
var askPage = helptext.Page{
	Usage: "claude-code ask [flags] <task | ->",
	About: "Run one task headlessly on an agent and stream its answer; an agent that " +
		"fails falls back per --fallback. With no task, or \"-\", the task is read from stdin.",
	Hint: "Every dispatch is logged: --log lists them, --retry <id> replays one.",
}

func cmdAsk(args []string) int { return runAsk(args, false) }

// runAsk is cmdAsk; resume makes the first positional the session id (`<agent> resume [flags] <id> <task>`).
func runAsk(args []string, resume bool) int {
	fs := helptext.NewFlagSet("ask", askPage)
	name := fs.String("agent", "claude", "vendor, agents/<name>.md, vendor:name, or an --agents key")
	var agents agentsFlag
	fs.Var(&agents, "agents", "agent definitions: --agent takes their keys")
	fallback := fs.String("fallback", router.FallbackAll, "fallback: all (agents, then claude)|agents|none")
	route := fs.String("route", "", "fallback ladder: default|first-party (default: by session profile)")
	native := fs.Bool("native", false, "opencode: skip the hub ladder; run on its default login")
	provider := fs.String("profile", "", "profile or hub start (default: pin, then registry)")
	as := fs.String("as", "", "claude on a paired Databricks profile: user|sp, over the profile's identity")
	model := fs.String("model", "", "alias (opus), version (opus-5-5) or wire id")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh|max")
	workdir := fs.String("workdir", "", "working directory for the agent")
	session := fs.String("session", "", "resume this session id")
	tools := fs.Bool("tools", false, "allow tool use where the agent can gate it (default: disabled)")
	readOnly := fs.Bool("read-only", false, "tools may read files but never write, run shell or commit; implies --tools")
	noTools := fs.Bool("no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	noPersist := fs.Bool("no-persist", false, "claude, codex: do not save the session for later resume")
	safeMode := fs.Bool("safe-mode", false, "claude, codex, opencode: skip hooks, plugins, customizations")
	debugFile := fs.String("debug-file", "", "claude, gemini: write the agent's debug log to this path")
	var attach attachFlag
	fs.Var(&attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	var keepEnv keepEnvFlag
	fs.Var(&keepEnv, "keep-env", "keep NAME[,NAME] from the credential scrub (repeatable)")
	var env envFlag
	fs.Var(&env, "env", "set NAME=VALUE in the agent's environment (repeatable); credential names are refused")
	systemPrompt := fs.String("system-prompt", "", "claude only: replace the system prompt")
	appendSystemPrompt := fs.String("append-system-prompt", "", "claude only: append to the system prompt")
	outputSchema := fs.String("output-schema", "", "JSON Schema (file or inline) the answer must satisfy; envelope gains structured")
	direct := fs.Bool("direct", false, "agents: hand the CLI the profile route, skip the proxy")
	autonomous := fs.Bool("autonomous", false, "drive the run without stopping to ask, per agent")
	enableMemory := fs.Bool("enable-memory", false, "copilot only: turn on session memory")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	stdinFramed := fs.Bool("stdin-framed", false, "internal: prompt is stdin up to a NUL byte; the run is cancelled when stdin then closes")
	host := fs.String("host", "", "run the ask on this ssh host (tailnet name or ssh alias); profiles resolve there")
	remoteBin := fs.String("remote-bin", "", "remote claude-code path for --host (default $"+agent.RemoteBinEnv+", then claude-code)")
	format := fs.String("format", "text", "stream text, print the JSON result envelope, or stream-json NDJSON events")
	rubric := fs.String("rubric", "", "review standard file quoted verbatim ahead of the task")
	retry := fs.String("retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	live := fs.Bool("live", false, "claude|gemini: hold a session open for agents send; --timeout is its idle limit")
	showLog := fs.Bool("log", false, "list the 20 most recent dispatches and exit")
	background := fs.Bool("background", false, "detach: log a job, print its id, exit")
	job := fs.String("job", "", "internal: run saved dispatch <id> in place")
	batchSrc := fs.String("batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	jobs := fs.Int("jobs", batch.DefaultJobs(), "batch: tasks run at once")
	failFast := fs.Bool("fail-fast", false, "batch: the first failed task cancels the rest")
	overlayPath := fs.String("overlay", "", "layer a JSON {set,unset,keep} file over the agent's env")
	overlayDebug := fs.String("overlay-debug", "", "record each agent child's env names + salted fingerprints (never values) to <file>")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := applyRoute(*route); err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if code := askEnableOverlays(*overlayDebug, *overlayPath); code != 0 {
		return code
	}
	fileOv, err := agentconfig.LoadOverlay("")
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	*tools = *tools || *readOnly // read-only tools are still tools
	if code, err := askCheckFlags(env, keepEnv, *format, *fallback, *as, *name, *background, *job, *tools, *noTools); err != nil {
		return failAskCode(*format, *name, err, code)
	}
	schema, err := agent.ResolveOutputSchema(*outputSchema)
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	defs, err := askDefs(agents, *host)
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	ref := *name // the raw selector Prepare resolves; *name becomes the agent so envelopes publish it
	ag, persona, err := agent.SelectAgent(ref, defs)
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	*name = ag
	if err := askCheckCombos(fs, *name, *live, persona != nil, *host, *background, *job, *batchSrc, len(env) > 0); err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if err := askOverlayLocal(fileOv, *live, *host); err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if *batchSrc != "" {
		base := agent.RunOptions{Model: *model, Provider: *provider, Effort: *effort, WorkDir: *workdir, Tools: *tools, ReadOnly: *readOnly, NoTools: *noTools,
			Timeout: *timeout, KeepEnv: []string(keepEnv), Autonomous: *autonomous, EnableMemory: *enableMemory, Direct: *direct,
			NoPersist: *noPersist, SafeMode: *safeMode, SystemPrompt: *systemPrompt, AppendSystemPrompt: *appendSystemPrompt,
			Env: []string(env), OutputSchema: schema, Agents: defs}
		return askBatch(fs, resume, *batchSrc, batchOpts{jobs: *jobs, failFast: *failFast, agent: ref, fallback: *fallback, native: *native},
			base, fileOv, env, keepEnv, *format, *name)
	}
	if helptext.Explicit(fs, "jobs") || helptext.Explicit(fs, "fail-fast") {
		return failAskCode(*format, *name, errors.New("--jobs and --fail-fast need --batch"), 2)
	}
	if *showLog {
		return printDispatchLog(*format)
	}
	positional, err := agent.AskPositional(fs.Args(), resume, session)
	if err != nil {
		return failAsk(*format, *name, err)
	}
	prompt := strings.Join(positional, " ")
	state := &askState{name: name, provider: provider, model: model, effort: effort, workdir: workdir,
		session: session, tools: tools, readOnly: readOnly, noTools: noTools, timeout: timeout, attach: &attach, keepEnv: &keepEnv,
		autonomous: autonomous, enableMemory: enableMemory, rubric: rubric, direct: direct,
		noPersist: noPersist, safeMode: safeMode, debugFile: debugFile, systemPrompt: systemPrompt,
		appendSystemPrompt: appendSystemPrompt, outputSchema: outputSchema, persona: &persona, host: host, remoteBin: remoteBin, fallback: fallback}
	jobRec, rc, done := loadAskJob(*job, *format, state, &prompt)
	if done {
		return rc
	}
	if jobRec == nil {
		if rc, done := loadAskRetry(fs, args, *retry, *format, state, &prompt); done {
			return rc
		}
	}
	if ref, ag, err = agent.AskReselect(ref, ag, name, &persona, defs); err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if err := askOverlayLocal(fileOv, *live, *host); err != nil { // a --retry/--job record may carry a --host
		return failAskCode(*format, *name, err, 2)
	}
	if schema, err = askRefreshSchema(*outputSchema, schema); err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if *live {
		return startLive(state, *name, *format)
	}
	if jobRec != nil && jobRec.Live {
		return runLiveHolder(jobRec)
	}
	framed, prompt, err := askReadPrompt(*stdinFramed, prompt)
	if err != nil {
		return failAsk(*format, *name, err)
	}
	if strings.TrimSpace(prompt) == "" {
		fs.Usage()
		printAskFailure(*format, *name, errors.New("no task: give a prompt, or \"-\" to read stdin"))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // a kill finalizes the record
	defer stop()
	if framed != nil { // the remote end of `ask --host`: stdin closing means the local side cancelled or vanished
		go func() { _, _ = io.Copy(io.Discard, framed); stop() }()
	}
	*timeout = askTimeout(fs, *timeout, *retry, *job, *tools)
	warnAskMemory(ag, *enableMemory)
	*debugFile = absDebugFile(*debugFile)
	req := agent.RunOptions{Prompt: prompt, Model: *model, Provider: *provider, Effort: *effort,
		WorkDir: *workdir, SessionID: *session, Tools: *tools, ReadOnly: *readOnly, NoTools: *noTools, Timeout: *timeout,
		Attach: []string(attach), KeepEnv: []string(keepEnv), Autonomous: *autonomous, EnableMemory: *enableMemory, Direct: *direct,
		NoPersist: *noPersist, SafeMode: *safeMode, DebugFile: *debugFile, SystemPrompt: *systemPrompt,
		AppendSystemPrompt: *appendSystemPrompt, Env: []string(env), OutputSchema: schema, Persona: persona,
		Agent: ref, Agents: defs, Fallback: *fallback, Native: *native, Host: *host, RemoteBin: *remoteBin, Rubric: *rubric, Background: *background}
	return askPrepareAndRun(ctx, fs, req, jobRec, fileOv, env, keepEnv, *as, *format, *name, *background, *native)
}

// askBatch layers the overlay over base and runs the --batch tasks.
func askBatch(fs *flag.FlagSet, resume bool, src string, o batchOpts, base agent.RunOptions, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, format, name string) int {
	ov, err := askOverlay(fileOv, env, keepEnv, false)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	return runAskBatch(fs, resume, src, o, agent.ApplyOverlay(base, ov))
}

// askRefreshSchema re-resolves raw, since a --retry/--job record may carry an already-resolved schema; empty keeps schema.
func askRefreshSchema(raw, schema string) (string, error) {
	if raw == "" {
		return schema, nil
	}
	return agent.ResolveOutputSchema(raw)
}

// askEnableOverlays turns on --overlay-debug and --overlay; it returns the exit code of a failure, else 0.
func askEnableOverlays(debugPath, overlayPath string) int {
	if debugPath != "" {
		if err := agent.EnableOverlayDebug(debugPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code ask: --overlay-debug:", err)
			return 2
		}
	}
	if overlayPath != "" {
		if err := agent.EnableOverlay(overlayPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code ask: --overlay:", err)
			return 2
		}
	}
	return 0
}

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches it or runs it and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, as, format, name string, background, native bool) int {
	ov, err := askOverlay(fileOv, env, keepEnv, background)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	req = agent.ApplyOverlay(req, ov)
	client := newClient()
	ctx = providers.WithIdentity(ctx, as)
	prepared, err := client.Prepare(ctx, req, jobRec)
	if err != nil {
		return failAskCode(format, name, err, prepareExitCode(err))
	}
	saveErr := prepared.Record.Save()
	if background {
		if saveErr != nil { // the --job child loads this record: without it the job could never start
			return failAsk(format, name, fmt.Errorf("save dispatch record: %w", saveErr))
		}
		return askSpawn(fs, prepared, native, format, name)
	}
	return runAskAndReport(ctx, client, prepared, jobRec != nil, format)
}

// askCheckFlags runs the flag checks needing no resolved agent; it returns the exit code with the error.
func askCheckFlags(env envFlag, keepEnv keepEnvFlag, format, fallback, as, name string, background bool, job string, tools, noTools bool) (int, error) {
	if _, err := agentconfig.FlagOverlay(env, keepEnv); err != nil { // names only: the error never carries a value
		return 2, err
	}
	if err := agent.ValidateAskFlags(format, fallback); err != nil {
		return 1, err
	}
	if err := agent.ValidateAs(as, name); err != nil {
		return 2, err
	}
	return 1, agent.AskExclusive(background, job, tools, noTools)
}

// askOverlay checks the --env/--keep-env shorthand and resolves the file overlay's vault refs, failing closed; a
// --background parent leaves the file to its --job child, which inherits $COWORKER_OVERLAY and resolves it itself.
func askOverlay(file agentconfig.Overlay, env, keep []string, background bool) (agentconfig.Resolved, error) {
	if _, err := agentconfig.FlagOverlay(env, keep); err != nil {
		return agentconfig.Resolved{}, err
	}
	if background {
		return agentconfig.Resolved{}, nil
	}
	return agentconfig.ResolveOverlay(file)
}

// askOverlayLocal refuses a file overlay on --live or --host: neither carries it, and --host would expose it to ps.
func askOverlayLocal(file agentconfig.Overlay, live bool, host string) error {
	if !file.Empty() && (live || host != "") {
		return errors.New("an overlay cannot reach --live or --host: run the agent locally")
	}
	return nil
}

// askDefs parses --agents; --host cannot carry an inline definition.
func askDefs(agents agentsFlag, host string) (map[string]*agent.Schema, error) {
	var defs map[string]*agent.Schema
	if agents != "" {
		var err error
		if defs, err = agent.ParseAgents(string(agents)); err != nil {
			return nil, err
		}
	}
	if host != "" && agents != "" {
		return nil, errors.New("--host cannot be combined with --agents: the remote cannot see an inline definition")
	}
	return defs, nil
}

// askCheckCombos rejects the flag combinations that need the resolved agent.
func askCheckCombos(fs *flag.FlagSet, name string, live, hasPersona bool, host string, background bool, job, batchSrc string, hasEnv bool) error {
	if live {
		if hasPersona {
			return errors.New("--live cannot run a persona (agents/<name>.md or an --agents key): use a plain vendor")
		}
		if err := askLiveCheck(fs, name); err != nil {
			return err
		}
	}
	if host != "" && (background || job != "" || batchSrc != "") {
		return errors.New("--host cannot be combined with --background, --job or --batch")
	}
	if hasEnv && background {
		return errors.New("--env cannot be combined with --background: env values are never stored in the dispatch record")
	}
	return nil
}

// askReadPrompt reads the framed stdin prompt, or resolves prompt; framed reader is nil when not framed.
func askReadPrompt(framed bool, prompt string) (*bufio.Reader, string, error) {
	if framed {
		return agent.ReadFramedPrompt(os.Stdin)
	}
	p, err := agent.ResolveAskPrompt(prompt)
	return nil, p, err
}

// askTimeout is the deadline: a fresh --tools run without an explicit --timeout gets 45m.
func askTimeout(fs *flag.FlagSet, timeout time.Duration, retry, job string, tools bool) time.Duration {
	if retry == "" && job == "" && tools && !helptext.Explicit(fs, "timeout") {
		return 45 * time.Minute
	}
	return timeout
}

func warnAskMemory(ag string, enableMemory bool) {
	if s, ok := agent.LookupSchema(ag); enableMemory && ok && s.CLI.Caps.Memory == agent.None {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s agent (copilot only)\n", ag)
	}
}

// prepareExitCode is the code a Prepare error names, else 1.
func prepareExitCode(err error) int {
	if usage, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		return usage.ExitCode()
	}
	return 1
}

// askSpawn detaches a prepared dispatch as a background job.
func askSpawn(fs *flag.FlagSet, prepared *agent.Prepared, native bool, format, name string) int {
	if err := job.Spawn(&prepared.Record, backgroundExtraFlags(fs, prepared.Opts.Fallback, native)); err != nil {
		return failAsk(format, name, err)
	}
	reportBackgroundStart(format, prepared.Record)
	return 0
}

// absDebugFile pins the path now: --workdir moves the child's cwd.
func absDebugFile(p string) string {
	if p == "" {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// runAskAndReport executes one prepared foreground dispatch and reports its outcome.
func runAskAndReport(ctx context.Context, client *agent.Client, p *agent.Prepared, job bool, format string) int {
	opts := agent.RunRecordedOpts{Job: job}
	if job && format == helptext.Text {
		opts.AnswerTo = os.Stdout // a --job child's stdout is the log: the full answer lands once at the end
	}
	res, err := client.Exec(ctx, p, askStdoutSink(format, p.Record.ID), opts)
	reportAskResult(format, res, err, res.DispatchID)
	for _, line := range []string{agent.RanOn(res), agent.DidNotRun(res)} {
		if line != "" {
			fmt.Fprintln(os.Stderr, "ask:", line)
		}
	}
	fmt.Fprintln(os.Stderr, "ask: dispatch", res.DispatchID, "(claude-code ask --retry <id> replays it)")
	if err != nil {
		return fail("ask: %v", err)
	}
	return 0
}

// The ask --format values.
const formatStreamJSON = agent.FormatStreamJSON

// askStdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func askStdoutSink(format, dispatchID string) agent.Sink {
	switch format {
	case helptext.JSON:
		return agent.Discard
	case formatStreamJSON:
		return agent.NewJSONLSink(os.Stdout, dispatchID)
	}
	return agent.NewTextSink(os.Stdout)
}

// askState bundles the ask flag pointers that --job/--retry overwrite from a logged dispatch's request.
type askState struct {
	name, provider, model, effort, workdir, session, rubric    *string
	tools, readOnly, noTools, autonomous, enableMemory, direct *bool
	noPersist, safeMode                                        *bool
	debugFile, systemPrompt, appendSystemPrompt, outputSchema  *string
	host, remoteBin, fallback                                  *string
	timeout                                                    *time.Duration
	persona                                                    **agent.Schema
	attach                                                     *attachFlag
	keepEnv                                                    *keepEnvFlag
}

// applyRecord copies a logged dispatch's request onto this ask invocation's flags.
func (s *askState) applyRecord(rec agent.Record) {
	*s.name, *s.provider, *s.model, *s.effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*s.workdir, *s.session, *s.tools, *s.timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*s.attach, *s.autonomous, *s.enableMemory = attachFlag(rec.Request.Attach), rec.Request.Autonomous, rec.Request.EnableMemory
	*s.keepEnv, *s.direct, *s.noTools, *s.readOnly = keepEnvFlag(rec.Request.KeepEnv), rec.Request.Direct, rec.Request.NoTools, rec.Request.ReadOnly
	*s.noPersist, *s.safeMode, *s.debugFile = rec.Request.NoPersist, rec.Request.SafeMode, rec.Request.DebugFile
	*s.systemPrompt, *s.appendSystemPrompt, *s.outputSchema = rec.Request.SystemPrompt, rec.Request.AppendSystemPrompt, rec.Request.OutputSchema
	*s.host, *s.remoteBin = rec.Host, rec.RemoteBin
	if s.persona != nil {
		*s.persona = rec.Request.Persona
	}
	if rec.FallbackPolicy != "" { // records from before the field keep this run's default
		*s.fallback = rec.FallbackPolicy
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(os.Stderr, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
}

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(job, format string, s *askState, prompt *string) (rec *agent.Record, rc int, done bool) {
	if job == "" {
		return nil, 0, false
	}
	r, err := agent.LoadRecord(job)
	if err != nil {
		return nil, failAsk(format, *s.name, err), true
	}
	if r.Status != "running" || r.PID != 0 {
		err := fmt.Errorf("--job %s: record is %s (pid %d), not a fresh background job", job, r.Status, r.PID)
		return nil, failAskCode(format, *s.name, err, 2), true
	}
	s.applyRecord(r)
	*prompt, *s.rubric = r.Request.Prompt, ""
	r.PID = os.Getpid()
	return &r, 0, false
}

// loadAskRetry loads a --retry record, then re-applies any flags explicitly typed on this command line.
func loadAskRetry(fs *flag.FlagSet, args []string, retry, format string, s *askState, prompt *string) (rc int, done bool) {
	if retry == "" {
		return 0, false
	}
	rec, err := agent.LoadRecord(retry)
	if err != nil {
		return failAsk(format, *s.name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.applyRecord(rec)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "attach" {
			*s.attach = nil
		}
		if f.Name == "keep-env" {
			*s.keepEnv = nil
		}
	})
	if err := fs.Parse(args); err != nil { // re-apply explicit flags over the record
		return failAskCode(format, *s.name, err, 2), true
	}
	*prompt, *s.rubric = rec.Request.Prompt, ""
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	return 0, false
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := agent.RecentRecords(20)
	if format == helptext.JSON {
		_ = helptext.WriteJSON(os.Stdout, recs)
		return 0
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
