package ask

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
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// askPage is `ask -h`; the task is the trailing args, or stdin when absent or "-".
var askPage = helptext.Page{
	Usage: "claude-code agents ask [flags] <task | ->",
	About: "Run one task headlessly on an agent and stream its answer; an agent that " +
		"fails falls back per --fallback. With no task, or \"-\", the task is read from stdin.",
	Hint: "Every dispatch is logged: --log lists them, --retry <id> replays one.",
}

func Cmd(args []string) int { return Run(args, false) }

// askFlags are the values of ask's flags.
type askFlags struct {
	name, fallback, route, provider, as, model, effort, workdir, session, debugFile, systemPrompt, appendSystemPrompt, outputSchema, host, remoteBin, format, rubric, retry, job, batchSrc, overlayPath, overlayDebug string
	native, tools, readOnly, noTools, noPersist, safeMode, direct, autonomous, fleet, enableMemory, stdinFramed, live, showLog, background, failFast                                                                  bool
	timeout                                                                                                                                                                                                           time.Duration
	jobs, maxFiles, maxFileBytes                                                                                                                                                                                      int
	dirs, files                                                                                                                                                                                                       string
	diff, index                                                                                                                                                                                                       bool
	pack                                                                                                                                                                                                              *pack.Spec
	agents                                                                                                                                                                                                            agentsFlag
	attach                                                                                                                                                                                                            attachFlag
	keepEnv                                                                                                                                                                                                           keepEnvFlag
	permAllow, permAsk, permDeny                                                                                                                                                                                      ruleFlag
	permMode                                                                                                                                                                                                          string
	env                                                                                                                                                                                                               envFlag
}

func askFlagSet() (*flag.FlagSet, *askFlags) {
	fs := helptext.NewFlagSet("ask", askPage)
	f := &askFlags{}
	fs.StringVar(&f.name, "agent", "claude", "vendor, agents/<name>.md, vendor:name, or an --agents key")
	fs.Var(&f.agents, "agents", "agent definitions: --agent takes their keys")
	fs.StringVar(&f.fallback, "fallback", router.FallbackAll, "fallback: all (agents, then claude)|agents|none")
	fs.StringVar(&f.route, "route", "", "fallback ladder: default|first-party (default: by session profile)")
	fs.BoolVar(&f.native, "native", false, "opencode: skip the hub ladder; run on its default login")
	fs.StringVar(&f.provider, "profile", "", "profile or hub start (default: pin, then registry)")
	fs.StringVar(&f.as, "as", "", "claude on a paired Databricks profile: user|sp, over the profile's identity")
	fs.StringVar(&f.model, "model", "", "alias (opus), version (opus-5-5) or wire id")
	fs.StringVar(&f.effort, "effort", "", "reasoning effort: low|medium|high|xhigh|max")
	fs.StringVar(&f.workdir, "workdir", "", "working directory for the agent")
	fs.StringVar(&f.session, "session", "", "resume this session id")
	fs.BoolVar(&f.tools, "tools", false, "allow tool use where the agent can gate it (default: disabled)")
	fs.BoolVar(&f.readOnly, "read-only", false, "tools may read files but never write, run shell or commit; implies --tools")
	fs.Var(&f.permAllow, "allow", "allow a tool call, e.g. 'Bash(ls:*)' (repeatable)")
	fs.Var(&f.permAsk, "ask", "ask before a tool call matching the rule (repeatable)")
	fs.Var(&f.permDeny, "deny", "deny a tool call matching the rule (repeatable)")
	fs.StringVar(&f.permMode, "mode", "", "permission mode for unmatched calls: default|auto|plan")
	fs.BoolVar(&f.noTools, "no-tools", false, "claude only: answer from the profile directly, no tools; not with --tools")
	fs.BoolVar(&f.noPersist, "no-persist", false, "claude, codex: do not save the session for later resume")
	fs.BoolVar(&f.safeMode, "safe-mode", false, "claude, codex, opencode: skip hooks, plugins, customizations")
	fs.StringVar(&f.debugFile, "debug-file", "", "claude, gemini: write the agent's debug log to this path")
	fs.Var(&f.attach, "attach", "attach a file (repeatable); task text precedes every attachment")
	fs.Var(&f.keepEnv, "keep-env", "keep NAME[,NAME] from the credential scrub (repeatable)")
	fs.Var(&f.env, "env", "set NAME=VALUE in the agent's environment (repeatable); credential names are refused")
	askRunFlags(fs, f)
	return fs, f
}

func askRunFlags(fs *flag.FlagSet, f *askFlags) {
	fs.StringVar(&f.systemPrompt, "system-prompt", "", "claude only: replace the system prompt")
	fs.StringVar(&f.appendSystemPrompt, "append-system-prompt", "", "claude only: append to the system prompt")
	fs.StringVar(&f.outputSchema, "output-schema", "", "JSON Schema (file or inline) the answer must satisfy; envelope gains structured")
	fs.BoolVar(&f.direct, "direct", false, "agents: hand the CLI the profile route, skip the proxy")
	fs.BoolVar(&f.autonomous, "autonomous", false, "drive the run without stopping to ask, per agent")
	fs.BoolVar(&f.fleet, "fleet", false, "copilot only: run the task in Copilot's fleet mode (parallel subagents)")
	fs.BoolVar(&f.enableMemory, "enable-memory", false, "copilot only: turn on session memory")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Minute, "per-attempt deadline (45m default with --tools)")
	fs.BoolVar(&f.stdinFramed, "stdin-framed", false, "internal: prompt is stdin up to a NUL byte; the run is cancelled when stdin then closes")
	fs.StringVar(&f.host, "host", "", "run the ask on this ssh host (tailnet name or ssh alias); profiles resolve there")
	fs.StringVar(&f.remoteBin, "remote-bin", "", "remote claude-code path for --host (default $"+agent.RemoteBinEnv+", then claude-code)")
	fs.StringVar(&f.format, "format", "text", "stream text, print the JSON result envelope, or stream-json NDJSON events")
	fs.StringVar(&f.dirs, "dirs", "", "comma-separated dirs whose tracked files are inlined")
	fs.StringVar(&f.files, "files", "", "comma-separated file globs to inline")
	fs.BoolVar(&f.diff, "diff", false, "inline the files changed vs HEAD")
	fs.BoolVar(&f.index, "index", false, "prepend an index of every matched path")
	fs.IntVar(&f.maxFiles, "max-files", pack.DefaultMaxFiles, "max files to inline (0 = index only)")
	fs.IntVar(&f.maxFileBytes, "max-file-bytes", pack.DefaultMaxFileBytes, "max bytes per inlined file")
	fs.StringVar(&f.rubric, "rubric", "", "review standard file quoted verbatim ahead of the task")
	fs.StringVar(&f.retry, "retry", "", "replay a logged dispatch by id (or unique id prefix); see --log")
	fs.BoolVar(&f.live, "live", false, "claude|gemini: hold a session open for agents send; --timeout is its idle limit")
	fs.BoolVar(&f.showLog, "log", false, "list the 20 most recent dispatches and exit")
	fs.BoolVar(&f.background, "background", false, "detach: log a job, print its id, exit")
	fs.StringVar(&f.job, "job", "", "internal: run saved dispatch <id> in place")
	fs.StringVar(&f.batchSrc, "batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	fs.IntVar(&f.jobs, "jobs", batch.DefaultJobs(), "batch: tasks run at once")
	fs.BoolVar(&f.failFast, "fail-fast", false, "batch: the first failed task cancels the rest")
	fs.StringVar(&f.overlayPath, "overlay", "", "layer a JSON {set,unset,keep} file over the agent's env")
	fs.StringVar(&f.overlayDebug, "overlay-debug", "", "record each agent child's env names + salted fingerprints (never values) to <file>")
}

// askRun is one ask invocation: its flags plus what the setup phases resolve.
type askRun struct {
	f       *askFlags
	fs      *flag.FlagSet
	args    []string
	resume  bool
	ref     string // the raw selector Prepare resolves; f.name becomes the agent so envelopes publish it
	ag      string
	persona *agent.Schema
	defs    map[string]*agent.Schema
	fileOv  agentconfig.Overlay
	schema  string
	jobRec  *agent.Record
	prompt  string
	state   *askState
	perms   agent.Permissions
}

// Run is Cmd; resume makes the first positional the session id (`<agent> resume [flags] <id> <task>`).
func Run(args []string, resume bool) int {
	fs, f := askFlagSet()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	r := &askRun{f: f, fs: fs, args: args, resume: resume}
	for _, phase := range []func() (int, bool){r.prelude, r.selectAgent, r.parsePerms, r.dispatchMode, r.resolveRecord, r.parsePerms} {
		if code, done := phase(); done {
			return code
		}
	}
	return r.execute()
}

func (r *askRun) fail(err error, code int) (int, bool) {
	return failAskCode(r.f.format, r.f.name, err, code), true
}

// prelude applies the route and overlays and validates the flags.
func (r *askRun) prelude() (int, bool) {
	f := r.f
	if err := ApplyRoute(f.route); err != nil {
		return r.fail(err, 2)
	}
	if code := askEnableOverlays(f.overlayDebug, f.overlayPath); code != 0 {
		return code, true
	}
	var err error
	if r.fileOv, err = agentconfig.LoadOverlay(""); err != nil {
		return r.fail(err, 2)
	}
	if f.pack, err = askPack(r.fs, f); err != nil {
		return r.fail(err, 2)
	}
	f.tools = f.tools || f.readOnly // read-only tools are still tools
	if code, err := askCheckFlags(f.env, f.keepEnv, f.format, f.fallback, f.as, f.name, f.background, f.job, f.tools, f.noTools); err != nil {
		return r.fail(err, code)
	}
	if r.schema, err = agent.ResolveOutputSchema(f.outputSchema); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// selectAgent resolves the --agent selector and checks the flag combinations it enables.
func (r *askRun) selectAgent() (int, bool) {
	f := r.f
	var err error
	if r.defs, err = agent.ParseAgentDefs(string(f.agents), f.host); err != nil {
		return r.fail(err, 2)
	}
	r.ref = f.name
	if r.ag, r.persona, err = agent.SelectAgent(r.ref, r.defs); err != nil {
		return r.fail(err, 2)
	}
	f.name = r.ag
	if err := askCheckCombos(r.fs, f.name, f.live, r.persona != nil, f.host, f.background, f.job, f.batchSrc, len(f.env) > 0); err != nil {
		return r.fail(err, 2)
	}
	if err := agent.OverlayLocal(r.fileOv, f.live, f.host); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// parsePerms validates --allow/--ask/--deny/--mode into r.perms; it runs again once a --retry record has filled them in.
func (r *askRun) parsePerms() (int, bool) {
	f := r.f
	p, err := agent.ParsePermissions(f.permMode, f.permAllow, f.permAsk, f.permDeny)
	if err != nil {
		return r.fail(err, 2)
	}
	r.perms = p
	return 0, false
}

// dispatchMode handles --batch and --log, which end the run before a single task is built.
func (r *askRun) dispatchMode() (int, bool) {
	f := r.f
	if f.batchSrc != "" {
		return askBatch(r.fs, r.resume, f.batchSrc, batchOpts{jobs: f.jobs, failFast: f.failFast, agent: r.ref, fallback: f.fallback, native: f.native},
			r.batchBase(), r.fileOv, f.env, f.keepEnv, f.format, f.name), true
	}
	if helptext.Explicit(r.fs, "jobs") || helptext.Explicit(r.fs, "fail-fast") {
		return r.fail(errors.New("--jobs and --fail-fast need --batch"), 2)
	}
	if f.showLog {
		return printDispatchLog(f.format), true
	}
	return 0, false
}

func (r *askRun) batchBase() agent.RunOptions {
	f := r.f
	return agent.RunOptions{Model: f.model, Provider: f.provider, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools,
		Timeout: f.timeout, KeepEnv: []string(f.keepEnv), Autonomous: f.autonomous, Fleet: f.fleet, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: []string(f.env), OutputSchema: r.schema, Agents: r.defs, Permissions: r.perms}
}

// resolveRecord reads the task and applies a --job or --retry record over the flags.
func (r *askRun) resolveRecord() (int, bool) {
	f := r.f
	positional, err := agent.AskPositional(r.fs.Args(), r.resume, &f.session)
	if err != nil {
		return failAsk(f.format, f.name, err), true
	}
	r.prompt = strings.Join(positional, " ")
	r.state = r.askState()
	var rc int
	var done bool
	if r.jobRec, rc, done = loadAskJob(f.job, f.format, r.state, &r.prompt); done {
		return rc, true
	}
	if r.jobRec == nil {
		if rc, done := loadAskRetry(r.fs, r.args, f.retry, f.format, r.state, &r.prompt); done {
			return rc, true
		}
	}
	if r.ref, r.ag, err = agent.AskReselect(r.ref, r.ag, &f.name, &r.persona, r.defs); err != nil {
		return r.fail(err, 2)
	}
	if err := agent.OverlayLocal(r.fileOv, f.live, f.host); err != nil { // a --retry/--job record may carry a --host
		return r.fail(err, 2)
	}
	if r.schema, err = askRefreshSchema(f.outputSchema, r.schema); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

func (r *askRun) askState() *askState {
	f := r.f
	st := &askState{Name: &f.name, Provider: &f.provider, Model: &f.model, Effort: &f.effort,
		Workdir: &f.workdir, Session: &f.session, Tools: &f.tools, ReadOnly: &f.readOnly, NoTools: &f.noTools,
		Timeout: &f.timeout, Attach: (*[]string)(&f.attach), KeepEnv: (*[]string)(&f.keepEnv), Autonomous: &f.autonomous,
		Fleet: &f.fleet, EnableMemory: &f.enableMemory, Direct: &f.direct, NoPersist: &f.noPersist, SafeMode: &f.safeMode,
		DebugFile: &f.debugFile, SystemPrompt: &f.systemPrompt, AppendSystemPrompt: &f.appendSystemPrompt,
		OutputSchema: &f.outputSchema, Persona: &r.persona, Host: &f.host, RemoteBin: &f.remoteBin, Fallback: &f.fallback,
		PermMode: &f.permMode, PermAllow: (*[]string)(&f.permAllow), PermAsk: (*[]string)(&f.permAsk), PermDeny: (*[]string)(&f.permDeny),
		rubric: &f.rubric, env: &f.env}
	st.Pack = &f.pack
	return st
}

// execute starts a live session or runs the one task.
func (r *askRun) execute() int {
	f := r.f
	if f.live {
		return startLive(r.state, f.name, f.format)
	}
	if r.jobRec != nil && r.jobRec.Live {
		return runLiveHolder(r.jobRec)
	}
	framed, prompt, err := askReadPrompt(f.stdinFramed, r.prompt)
	if err != nil {
		return failAsk(f.format, f.name, err)
	}
	if strings.TrimSpace(prompt) == "" {
		r.fs.Usage()
		printAskFailure(f.format, f.name, errors.New("no task: give a prompt, or \"-\" to read stdin"))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // a kill finalizes the record
	defer stop()
	if framed != nil { // the remote end of `ask --host`: stdin closing means the local side cancelled or vanished
		go func() { _, _ = io.Copy(io.Discard, framed); stop() }()
	}
	f.timeout = askTimeout(r.fs, f.timeout, f.retry, f.job, f.tools)
	warnAskMemory(r.ag, f.enableMemory)
	f.debugFile = absDebugFile(f.debugFile)
	return askPrepareAndRun(ctx, r.fs, r.request(prompt), r.jobRec, r.fileOv, f.env, f.keepEnv, f.as, f.format, f.name, f.background, f.native)
}

func (r *askRun) request(prompt string) agent.RunOptions {
	f := r.f
	return agent.RunOptions{Prompt: prompt, Model: f.model, Provider: f.provider, Effort: f.effort,
		WorkDir: f.workdir, SessionID: f.session, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools, Timeout: f.timeout,
		Attach: []string(f.attach), KeepEnv: []string(f.keepEnv), Autonomous: f.autonomous, Fleet: f.fleet, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, DebugFile: f.debugFile, SystemPrompt: f.systemPrompt,
		AppendSystemPrompt: f.appendSystemPrompt, Env: []string(f.env), OutputSchema: r.schema, Persona: r.persona,
		Pack: f.pack, Agent: r.ref, Agents: r.defs, Fallback: f.fallback, Native: f.native, Host: f.host, RemoteBin: f.remoteBin, Rubric: f.rubric, Background: f.background, Permissions: r.perms}
}

// askBatch layers the overlay over base and runs the --batch tasks.
func askBatch(fs *flag.FlagSet, resume bool, src string, o batchOpts, base agent.RunOptions, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, format, name string) int {
	ov, err := agent.ResolveOverlay(fileOv, env, keepEnv, false)
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
		if strings.HasPrefix(debugPath, "-") {
			fmt.Fprintf(os.Stderr, "claude-code agents ask: --overlay-debug: %q looks like a flag; give a file path\n", debugPath)
			return 2
		}
		if err := agent.EnableOverlayDebug(debugPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code agents ask: --overlay-debug:", err)
			return 2
		}
	}
	if overlayPath != "" {
		if err := agent.EnableOverlay(overlayPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code agents ask: --overlay:", err)
			return 2
		}
	}
	return 0
}

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches it or runs it and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, as, format, name string, background, native bool) int {
	ov, err := agent.ResolveOverlay(fileOv, env, keepEnv, background)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	req = agent.ApplyOverlay(req, ov)
	client := NewClient()
	ctx = providers.WithIdentity(ctx, as)
	prepared, err := client.Prepare(ctx, req, jobRec)
	if err != nil {
		return failAskCode(format, name, err, prepareExitCode(err))
	}
	saveErr := prepared.Record.Save()
	if saveErr != nil && !background {
		fmt.Fprintf(os.Stderr, "ask: warning: dispatch record not saved, --retry may not find it: %v\n", saveErr)
	}
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

// askPack builds the pack spec from --dirs/--files/--diff; nil when none is set. The tuning flags need one.
func askPack(fs *flag.FlagSet, f *askFlags) (*pack.Spec, error) {
	if f.dirs == "" && f.files == "" && !f.diff {
		for _, n := range []string{"index", "max-files", "max-file-bytes"} {
			if helptext.Explicit(fs, n) {
				return nil, fmt.Errorf("--%s needs --dirs, --files or --diff", n)
			}
		}
		return nil, nil
	}
	if f.maxFiles < 0 || f.maxFileBytes < 0 {
		return nil, errors.New("--max-files and --max-file-bytes cannot be negative")
	}
	if f.batchSrc != "" || f.live || f.host != "" {
		return nil, errors.New("--dirs/--files/--diff cannot be combined with --batch, --live or --host")
	}
	return &pack.Spec{Dirs: textutil.SplitCSV(f.dirs), Files: textutil.SplitCSV(f.files), Diff: f.diff, Index: f.index,
		MaxFiles: f.maxFiles, MaxFileBytes: f.maxFileBytes, IndexOnly: f.maxFiles == 0}, nil
}

// askCheckCombos rejects the flag combinations that need the resolved agent.
func askCheckCombos(fs *flag.FlagSet, name string, live, hasPersona bool, host string, background bool, job, batchSrc string, hasEnv bool) error {
	if live {
		if err := askLiveCombos(fs, name, hasPersona); err != nil {
			return err
		}
	}
	return askHostEnvCombos(host, background, job, batchSrc, hasEnv)
}

// askLiveCombos rejects --live with a persona, then checks the flags --live cannot take.
func askLiveCombos(fs *flag.FlagSet, name string, hasPersona bool) error {
	if hasPersona {
		return errors.New("--live cannot run a persona (agents/<name>.md or an --agents key): use a plain vendor")
	}
	return askLiveCheck(fs, name)
}

// askHostEnvCombos rejects --host with --background/--job/--batch, and --env with --background.
func askHostEnvCombos(host string, background bool, job, batchSrc string, hasEnv bool) error {
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
	ReportBackgroundStart(format, prepared.Record)
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
	res, err := client.Exec(ctx, p, StdoutSink(format, p.Record.ID), opts)
	reportAskResult(format, res, err, res.DispatchID)
	for _, line := range []string{agent.RanOn(res), agent.DidNotRun(res)} {
		if line != "" {
			fmt.Fprintln(os.Stderr, "ask:", line)
		}
	}
	fmt.Fprintln(os.Stderr, "ask: dispatch", res.DispatchID, "(agents result <id> reprints it; agents ask --retry <id> replays it)")
	if err != nil {
		return helptext.Fail("ask: %v", err)
	}
	return 0
}

// The ask --format values.
const formatStreamJSON = agent.FormatStreamJSON

// StdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func StdoutSink(format, dispatchID string) agent.Sink {
	switch format {
	case helptext.JSON:
		return agent.Discard
	case formatStreamJSON:
		return agent.NewJSONLSink(os.Stdout, dispatchID)
	}
	return agent.NewTextSink(os.Stdout)
}

// askState is the agent.AskTarget plus the flag pointers --retry resets or --job clears.
type askState struct {
	agent.AskTarget
	rubric *string
	env    *envFlag
}

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(job, format string, s *askState, prompt *string) (rec *agent.Record, rc int, done bool) {
	if job == "" {
		return nil, 0, false
	}
	r, err := agent.LoadJobRecord(job)
	if errors.Is(err, agent.ErrJobNotFresh) {
		return nil, failAskCode(format, *s.Name, err, 2), true
	}
	if err != nil {
		return nil, failAsk(format, *s.Name, err), true
	}
	s.Apply(r, os.Stderr)
	*prompt, *s.rubric = r.Request.Prompt, ""
	return &r, 0, false
}

// loadAskRetry loads a --retry record, then re-applies any flags explicitly typed on this command line.
func loadAskRetry(fs *flag.FlagSet, args []string, retry, format string, s *askState, prompt *string) (rc int, done bool) {
	if retry == "" {
		return 0, false
	}
	rec, err := agent.LoadRecord(retry)
	if err != nil {
		return failAsk(format, *s.Name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	s.Apply(rec, os.Stderr)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "attach" {
			*s.Attach = nil
		}
		if f.Name == "keep-env" {
			*s.KeepEnv = nil
		}
		if f.Name == "allow" {
			*s.PermAllow = nil
		}
		if f.Name == "ask" {
			*s.PermAsk = nil
		}
		if f.Name == "deny" {
			*s.PermDeny = nil
		}
		if f.Name == "env" {
			*s.env = nil
		}
	})
	if err := fs.Parse(args); err != nil { // re-apply explicit flags over the record
		return failAskCode(format, *s.Name, err, 2), true
	}
	*prompt, *s.rubric = rec.Request.Prompt, ""
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	return 0, false
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := agent.RecentRecords(20)
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("ask", recs)
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
