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
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/launchplan"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
	jobRec  *job.Record
	prompt  string
	state   *askState
	perms   agent.Permissions
	lead    string // profile a routed run leads with (--profile session, --provider, or --profile with --fallback)
}

// Run is Cmd; resume makes the first positional the session id (`<agent> resume [flags] <id> <task>`).
func Run(args []string, resume bool) int {
	fs, f := askFlagSet()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	r := &askRun{f: f, fs: fs, args: args, resume: resume}
	for _, phase := range []func() (int, bool){r.prelude, r.selectAgent, r.parsePerms, r.dispatchMode, r.resolveRecord, r.resolveLead, r.parsePerms} {
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
	if code, err := askCheckFlags(f.env, f.keepEnv, f.format, f.fallback, f.background, f.job, f.tools, f.noTools); err != nil {
		return r.fail(err, code)
	}
	if r.schema, err = client.ResolveOutputSchema(f.outputSchema); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// selectAgent resolves the --agent selector and checks the flag combinations it enables.
func (r *askRun) selectAgent() (int, bool) {
	f := r.f
	var err error
	if r.defs, err = client.ParseAgentDefs(string(f.agents), f.host); err != nil {
		return r.fail(err, 2)
	}
	r.ref = f.name
	if r.ag, r.persona, err = agent.SelectAgent(r.ref, r.defs); err != nil {
		return r.fail(err, 2)
	}
	f.name = r.ag
	if err := client.ValidateAs(f.as, f.name); err != nil { // against the resolved agent: a claude persona passes
		return r.fail(err, 2)
	}
	if err := askCheckCombos(r.fs, f.name, f.live, r.persona != nil, f.host, f.background, f.job, f.batchSrc, len(f.env) > 0); err != nil {
		return r.fail(err, 2)
	}
	if err := client.OverlayLocal(r.fileOv, f.live, f.host); err != nil {
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
		retryAgents, err := r.retryAgents()
		if err != nil {
			return r.fail(err, 2)
		}
		if code, done := r.resolveLead(); done { // validates --provider/--profile and sets r.lead for batchBase
			return code, true
		}
		o := Opts{Jobs: f.jobs, FailFast: f.failFast, Agent: r.ref, Fallback: f.fallback, Native: f.native,
			FollowInput: f.follow, FollowIdle: f.followIdle, RetryAgents: retryAgents}
		return askBatch(r.fs, r.resume, f.batchSrc, o,
			r.batchBase(), r.fileOv, f.env, f.keepEnv, f.format, f.name), true
	}
	for _, name := range []string{"jobs", "fail-fast", "follow", "follow-idle", "retry-agent"} {
		if helptext.Explicit(r.fs, name) {
			return r.fail(fmt.Errorf("--%s needs --batch", name), 2)
		}
	}
	if f.showLog {
		return printDispatchLog(f.format), true
	}
	return 0, false
}

// retryAgents resolves --retry-agent names like --agent does; a bad name is a flag error.
func (r *askRun) retryAgents() ([]string, error) {
	var out []string
	for n := range strings.SplitSeq(r.f.retryAgent, ",") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		name, _, err := agent.SelectAgent(n, r.defs)
		if err != nil {
			return nil, fmt.Errorf("--retry-agent: %w", err)
		}
		out = append(out, name)
	}
	return out, nil
}

func (r *askRun) batchBase() agent.RunOptions {
	f := r.f
	return agent.RunOptions{Model: f.model, Provider: f.provider, Lead: r.lead, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools,
		Timeout: f.timeout, KeepEnv: []string(f.keepEnv), Autonomous: f.autonomous, Fleet: f.fleet, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: []string(f.env), OutputSchema: r.schema, Agents: r.defs, Permissions: r.perms}
}

// resolveRecord reads the task and applies a --job or --retry record over the flags.
func (r *askRun) resolveRecord() (int, bool) {
	f := r.f
	positional, err := client.AskPositional(r.fs.Args(), r.resume, &f.session)
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
	if r.ref, r.ag, err = client.AskReselect(r.ref, r.ag, &f.name, &r.persona, r.defs); err != nil {
		return r.fail(err, 2)
	}
	if err := client.OverlayLocal(r.fileOv, f.live, f.host); err != nil { // a --retry/--job record may carry a --host
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
	st.lead = &r.lead
	return st
}

// execute starts a live session or runs the one task.
func (r *askRun) execute() int {
	f := r.f
	if f.live {
		return startLive(r.state, r.perms, f.name, f.as, f.format)
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
	return agent.RunOptions{Prompt: prompt, Model: f.model, Provider: f.provider, Lead: r.lead, Effort: f.effort,
		WorkDir: f.workdir, SessionID: f.session, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools, Timeout: f.timeout,
		Attach: []string(f.attach), KeepEnv: []string(f.keepEnv), Autonomous: f.autonomous, Fleet: f.fleet, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, DebugFile: f.debugFile, SystemPrompt: f.systemPrompt,
		AppendSystemPrompt: f.appendSystemPrompt, Env: []string(f.env), OutputSchema: r.schema, Persona: r.persona,
		Pack: f.pack, Agent: r.ref, Agents: r.defs, Fallback: f.fallback, Native: f.native, Host: f.host, RemoteBin: f.remoteBin, Rubric: f.rubric, Background: f.background, Permissions: r.perms}
}

// askBatch layers the overlay over base and runs the --batch tasks.
func askBatch(fs *flag.FlagSet, resume bool, src string, o Opts, base agent.RunOptions, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, format, name string) int {
	ov, err := client.ResolveOverlay(fileOv, env, keepEnv, false)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	return runAskBatch(fs, resume, src, o, launchplan.ApplyOverlay(base, ov))
}

// askRefreshSchema re-resolves raw, since a --retry/--job record may carry an already-resolved schema; empty keeps schema.
func askRefreshSchema(raw, schema string) (string, error) {
	if raw == "" {
		return schema, nil
	}
	return client.ResolveOutputSchema(raw)
}

// askEnableOverlays turns on --overlay-debug and --overlay; it returns the exit code of a failure, else 0.
func askEnableOverlays(debugPath, overlayPath string) int {
	if debugPath != "" {
		if strings.HasPrefix(debugPath, "-") {
			fmt.Fprintf(os.Stderr, "claude-code agents ask: --overlay-debug: %q looks like a flag; give a file path\n", debugPath)
			return 2
		}
		if err := agentconfig.EnableOverlayDebug(debugPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code agents ask: --overlay-debug:", err)
			return 2
		}
	}
	if overlayPath != "" {
		if err := launchplan.EnableOverlay(overlayPath); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code agents ask: --overlay:", err)
			return 2
		}
	}
	return 0
}

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches it or runs it and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *job.Record, fileOv agentconfig.Overlay,
	env envFlag, keepEnv keepEnvFlag, as, format, name string, background, native bool) int {
	ov, err := client.ResolveOverlay(fileOv, env, keepEnv, background)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	req = launchplan.ApplyOverlay(req, ov)
	cl := NewClient()
	ctx = providers.WithIdentity(ctx, as)
	prepared, err := cl.Prepare(ctx, req, jobRec)
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
	return runAskAndReport(ctx, cl, prepared, jobRec != nil, format)
}

// askReadPrompt reads the framed stdin prompt, or resolves prompt; framed reader is nil when not framed.
func askReadPrompt(framed bool, prompt string) (*bufio.Reader, string, error) {
	if framed {
		return client.ReadFramedPrompt(os.Stdin)
	}
	p, err := client.ResolveAskPrompt(prompt)
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

// resolveLead turns --profile session, --provider, or --profile with an explicit --fallback into a routed lead.
func (r *askRun) resolveLead() (int, bool) {
	f := r.f
	fallbackSet := false
	r.fs.Visit(func(fl *flag.Flag) { fallbackSet = fallbackSet || fl.Name == "fallback" })
	// --fallback none has nothing to fall back to, so a named --profile keeps pinning one route
	if f.leadProvider == "" && f.provider != agent.LeadSession && (f.provider == "" || !fallbackSet || f.fallback == router.FallbackNone) {
		return 0, false
	}
	reg, err := providers.Load()
	if err != nil {
		return r.fail(err, 1)
	}
	lead, err := agent.ResolveLead(reg, r.ag, f.provider, f.leadProvider)
	if err != nil {
		return r.fail(err, 2)
	}
	r.lead, f.provider = lead, ""
	return 0, false
}
