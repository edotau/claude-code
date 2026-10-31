package cli

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/crossgen"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
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
	fallback := fs.String("fallback", agent.FallbackAll, "fallback: all (agents, then claude)|agents|none")
	native := fs.Bool("native", false, "opencode: skip the hub ladder; run on its default login")
	provider := fs.String("profile", "", "profile or hub start (default: pin, then registry)")
	model := fs.String("model", "", "alias (opus), version (opus-5-5) or wire id")
	effort := fs.String("effort", "", "reasoning effort: low|medium|high|xhigh|max")
	workdir := fs.String("workdir", "", "working directory for the agent")
	session := fs.String("session", "", "resume this session id")
	tools := fs.Bool("tools", false, "allow tool use where the agent can gate it (default: disabled)")
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
	batch := fs.String("batch", "", "run JSONL tasks from <file> or - on a pool; one envelope per line")
	jobs := fs.Int("jobs", agent.DefaultJobs(), "batch: tasks run at once")
	failFast := fs.Bool("fail-fast", false, "batch: the first failed task cancels the rest")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := agent.ValidateEnv(env); err != nil { // names only: the error never carries a value
		return failAskCode(*format, *name, err, 2)
	}
	if err := validateAskFlags(*format, *fallback); err != nil {
		return failAsk(*format, *name, err)
	}
	if err := askExclusive(*background, *job, *tools, *noTools); err != nil {
		return failAsk(*format, *name, err)
	}
	schema, err := agent.ResolveOutputSchema(*outputSchema)
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	var defs map[string]*agent.Schema
	if agents != "" {
		if defs, err = agent.ParseAgents(string(agents)); err != nil {
			return failAskCode(*format, *name, err, 2)
		}
	}
	if *host != "" && agents != "" {
		return failAskCode(*format, *name, errors.New("--host cannot be combined with --agents: the remote cannot see an inline definition"), 2)
	}
	vendor, persona, err := agent.SelectAgent(*name, defs)
	if err != nil {
		return failAskCode(*format, *name, err, 2)
	}
	if *live {
		if persona != nil {
			return failAskCode(*format, *name, errors.New("--live cannot run a persona (agents/<name>.md or an --agents key): use a plain vendor"), 2)
		}
		if err := askLiveCheck(fs, *name); err != nil {
			return failAskCode(*format, *name, err, 2)
		}
	}
	if *host != "" && (*background || *job != "" || *batch != "") {
		return failAskCode(*format, *name, errors.New("--host cannot be combined with --background, --job or --batch"), 2)
	}
	if len(env) > 0 && *background {
		return failAskCode(*format, *name, errors.New("--env cannot be combined with --background: env values are never stored in the dispatch record"), 2)
	}
	if *batch != "" {
		base := agent.RunOptions{Model: *model, Provider: *provider, Effort: *effort, WorkDir: *workdir, Tools: *tools, NoTools: *noTools,
			Timeout: *timeout, KeepEnv: []string(keepEnv), Autonomous: *autonomous, EnableMemory: *enableMemory, Direct: *direct,
			NoPersist: *noPersist, SafeMode: *safeMode, SystemPrompt: *systemPrompt, AppendSystemPrompt: *appendSystemPrompt,
			Env: []string(env), OutputSchema: schema, Agents: defs}
		return runAskBatch(fs, resume, *batch, batchOpts{jobs: *jobs, failFast: *failFast, agent: *name, fallback: *fallback, native: *native}, base)
	}
	if flagExplicit(fs, "jobs") || flagExplicit(fs, "fail-fast") {
		return failAskCode(*format, *name, errors.New("--jobs and --fail-fast need --batch"), 2)
	}
	if *showLog {
		return printDispatchLog(*format)
	}
	positional := fs.Args()
	if resume {
		if len(positional) < 1 {
			return failAsk(*format, *name, errors.New("resume needs a session id: <agent> resume [flags] <id> <task>"))
		}
		*session, positional = positional[0], positional[1:]
	}
	prompt := strings.Join(positional, " ")
	state := &askState{name: name, provider: provider, model: model, effort: effort, workdir: workdir,
		session: session, tools: tools, noTools: noTools, timeout: timeout, attach: &attach, keepEnv: &keepEnv,
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
	if rest, p, rerr := agent.SelectAgent(*name, defs); rerr != nil { // a --retry re-parse puts the raw --agent value back
		return failAskCode(*format, *name, rerr, 2)
	} else {
		vendor = rest
		if p != nil { // an explicit persona beats the record's
			persona = p
		}
	}
	if *outputSchema != "" { // a --retry/--job record may carry an already-resolved schema
		if schema, err = agent.ResolveOutputSchema(*outputSchema); err != nil {
			return failAskCode(*format, *name, err, 2)
		}
	}
	if *live {
		return startLive(state, *name, *format)
	}
	if jobRec != nil && jobRec.Live {
		return runLiveHolder(jobRec)
	}
	var framed *bufio.Reader
	if *stdinFramed {
		framed, prompt, err = readFramedPrompt(os.Stdin)
	} else {
		prompt, err = resolveAskPrompt(prompt)
	}
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
	if *retry == "" && *job == "" && *tools && !timeoutExplicit(fs) {
		*timeout = 45 * time.Minute
	}
	if s, ok := agent.LookupSchema(vendor); *enableMemory && ok && s.CLI.Caps.Memory == agent.None {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s agent (copilot only)\n", vendor)
	}
	*debugFile = absDebugFile(*debugFile)
	req := agent.RunOptions{Prompt: prompt, Model: *model, Provider: *provider, Effort: *effort,
		WorkDir: *workdir, SessionID: *session, Tools: *tools, NoTools: *noTools, Timeout: *timeout,
		Attach: []string(attach), KeepEnv: []string(keepEnv), Autonomous: *autonomous, EnableMemory: *enableMemory, Direct: *direct,
		NoPersist: *noPersist, SafeMode: *safeMode, DebugFile: *debugFile, SystemPrompt: *systemPrompt,
		AppendSystemPrompt: *appendSystemPrompt, Env: []string(env), OutputSchema: schema, Persona: persona,
		Agent: *name, Agents: defs, Fallback: *fallback, Native: *native, Host: *host, RemoteBin: *remoteBin, Rubric: *rubric, Background: *background}
	client := newClient()
	prepared, err := client.Prepare(ctx, req, jobRec)
	if err != nil {
		code := 1
		if usage, ok := errors.AsType[interface {
			error
			ExitCode() int
		}](err); ok {
			code = usage.ExitCode()
		}
		return failAskCode(*format, *name, err, code)
	}
	_ = prepared.Record.Save()
	if *background {
		if err := agent.Spawn(&prepared.Record, backgroundExtraFlags(fs, prepared.Opts.Fallback, *native)); err != nil {
			return failAsk(*format, *name, err)
		}
		reportBackgroundStart(*format, prepared.Record)
		return 0
	}
	return runAskAndReport(ctx, client, prepared, jobRec != nil, *format)
}

// askExclusive rejects flag pairs that cannot be combined.
func askExclusive(background bool, job string, tools, noTools bool) error {
	if background && job != "" {
		return errors.New("--background and --job are mutually exclusive")
	}
	if tools && noTools {
		return errors.New("--tools and --no-tools are mutually exclusive")
	}
	return nil
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
	opts := agent.DispatchOpts{Job: job}
	if job && format == formatText {
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
const (
	formatText       = "text"
	formatJSON       = "json"
	formatStreamJSON = "stream-json"
)

// askStdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func askStdoutSink(format, dispatchID string) agent.Sink {
	switch format {
	case formatJSON:
		return agent.Discard
	case formatStreamJSON:
		return agent.NewJSONLSink(os.Stdout, dispatchID)
	}
	return agent.NewTextSink(os.Stdout)
}

// askState bundles the ask flag pointers that --job/--retry overwrite from a logged dispatch's request.
type askState struct {
	name, provider, model, effort, workdir, session, rubric   *string
	tools, noTools, autonomous, enableMemory, direct          *bool
	noPersist, safeMode                                       *bool
	debugFile, systemPrompt, appendSystemPrompt, outputSchema *string
	host, remoteBin, fallback                                 *string
	timeout                                                   *time.Duration
	persona                                                   **agent.Schema
	attach                                                    *attachFlag
	keepEnv                                                   *keepEnvFlag
}

// applyRecord copies a logged dispatch's request onto this ask invocation's flags.
func (s *askState) applyRecord(rec agent.Record) {
	*s.name, *s.provider, *s.model, *s.effort = rec.Agent, rec.Request.Provider, rec.Request.Model, rec.Request.Effort
	*s.workdir, *s.session, *s.tools, *s.timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	*s.attach, *s.autonomous, *s.enableMemory = attachFlag(rec.Request.Attach), rec.Request.Autonomous, rec.Request.EnableMemory
	*s.keepEnv, *s.direct, *s.noTools = keepEnvFlag(rec.Request.KeepEnv), rec.Request.Direct, rec.Request.NoTools
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
		fmt.Fprintf(os.Stderr, "ask: the logged run set env %s; values are not stored, so pass --env again\n", strings.Join(rec.Request.EnvNames, ","))
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

// attachFlag collects repeatable --attach paths; Set resolves each to an absolute path and fails fast
// unless it names an existing regular file.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

// Placeholder names the FLAGS placeholder for attachFlag (helptext.Placeholder).
func (a *attachFlag) Placeholder() string { return "PATH" }

func (a *attachFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--attach %q: not a regular file", s)
	}
	// reject non-image binary up front: it fails the same way on every runner, so it must never reach the fallback chain
	if data, err := os.ReadFile(abs); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(data) && !agent.IsImage(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
}

// keepEnvFlag collects repeatable --keep-env NAME[,NAME] values, comma-split per occurrence.
type keepEnvFlag []string

func (k *keepEnvFlag) String() string { return strings.Join(*k, ",") }

// Placeholder names the FLAGS placeholder for keepEnvFlag (helptext.Placeholder).
func (k *keepEnvFlag) Placeholder() string { return "NAME,NAME" }

func (k *keepEnvFlag) Set(s string) error {
	*k = append(*k, strings.Split(s, ",")...)
	return nil
}

// envFlag collects repeatable --env NAME=VALUE entries.
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

// Placeholder names the FLAGS placeholder for envFlag (helptext.Placeholder).
func (e *envFlag) Placeholder() string { return "NAME=VALUE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so runAsk validates after Parse.
func (e *envFlag) Set(s string) error {
	*e = append(*e, s)
	return nil
}

// agentsFlag is the --agents value: inline JSON or a file of agent definitions.
type agentsFlag string

func (a *agentsFlag) String() string { return string(*a) }

// Placeholder names the FLAGS placeholder for agentsFlag (helptext.Placeholder).
func (a *agentsFlag) Placeholder() string { return "JSON|FILE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so runAsk validates after Parse.
func (a *agentsFlag) Set(s string) error {
	*a = agentsFlag(s)
	return nil
}

// flagExplicit is true when --<name> appeared on this command line.
func flagExplicit(fs *flag.FlagSet, name string) bool {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			explicit = true
		}
	})
	return explicit
}

// timeoutExplicit is true when --timeout appeared on this command line, so it always beats the
// tools-aware default.
func timeoutExplicit(fs *flag.FlagSet) bool {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "timeout" {
			explicit = true
		}
	})
	return explicit
}

func validateAskFlags(format, fallback string) error {
	if !slices.Contains([]string{formatText, formatJSON, formatStreamJSON}, format) {
		return fmt.Errorf("--format %q unknown (text|json|stream-json)", format)
	}
	if !slices.Contains(agent.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(agent.FallbackModes, "|"))
	}
	return nil
}

// readFramedPrompt reads the prompt up to the NUL that ends it and returns the reader, whose remaining EOF is the cancel signal.
func readFramedPrompt(in io.Reader) (*bufio.Reader, string, error) {
	rd := bufio.NewReader(in)
	p, err := rd.ReadString(0)
	if err != nil {
		return nil, "", fmt.Errorf("--stdin-framed: stdin ended before the NUL that ends the prompt: %w", err)
	}
	return rd, strings.TrimSuffix(p, "\x00"), nil
}

// resolveAskPrompt reads stdin when the prompt is empty or the literal "-".
func resolveAskPrompt(prompt string) (string, error) {
	if prompt != "" && prompt != "-" {
		return prompt, nil
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}

func newClient() *agent.Client {
	return agent.NewClient(launch.Launcher{}, hooks.RuleClassifier{}.Tier, routeRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}

// backgroundExtraFlags carries explicitly-set --fallback/--native over to the spawned --job child;
// unset flags fall back to the child's own defaults instead of being re-pinned.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "fallback":
			extra = append(extra, "--fallback", fallback)
		case "native":
			if native {
				extra = append(extra, "--native")
			}
		}
	})
	return extra
}

// reportBackgroundStart tells the caller how to track a detached job.
func reportBackgroundStart(format string, rec agent.Record) {
	if format == formatJSON {
		body, _ := json.Marshal(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
		fmt.Println(string(body))
		return
	}
	fmt.Printf("job %s started — claude-code agents status|tail|result|cancel %s\n", rec.ID, rec.ID)
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json.
func printAskFailure(format, agentName string, err error) {
	switch format {
	case formatJSON:
		body, _ := json.Marshal(agent.FailureEnvelope(agentName, err))
		fmt.Println(string(body))
	case formatStreamJSON:
		_ = askStdoutSink(format, "").Emit(agent.ResultEvent(agent.FailureEnvelope(agentName, err)))
	}
}

func reportAskResult(format string, res agent.Result, err error, dispatchID string) {
	for _, d := range res.Dropped {
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == formatJSON {
		body, _ := json.Marshal(agent.NewEnvelope(res, err, dispatchID))
		fmt.Println(string(body))
		return
	}
	if format == formatStreamJSON {
		return // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
}

// agentsPage is `agents -h`; bare `agents` lists the runners.
var agentsPage = helptext.Page{
	Usage: "claude-code agents [<command>] [flags]",
	About: "With no command, list each agent, whether it is available and which " +
		"request fields (model, profile, effort, …) it honors.",
	Commands: [][2]string{
		{"sync", "project agents/ + CLAUDE.md into every agent home; link skills"},
		{"tree", "live tree of sessions, subagents and dispatches; --watch"},
		{"reap", "mark dead running dispatches as error; drop stale markers"},
		{"status", "show a dispatch's state (any agent): status <id>"},
		{"result", "print a finished dispatch's answer or envelope: result <id>"},
		{"tail", "follow a dispatch's log or events: tail <id> [-f]"},
		{"cancel", "interrupt a running dispatch: cancel <id>"},
		{"send", "send one turn to a live claude or gemini session: send <id> <task>"},
	},
	Hint: "claude-code agents <command> -h lists that command's flags.",
}

// agentsSyncPage is `agents sync -h`.
var agentsSyncPage = helptext.Page{
	Usage: "claude-code agents sync [flags]",
	About: "Render agents/ into state/projections/<agent>/agents plus each agent's instruction " +
		"file, install them into the agent homes, and link skills/ into ~/.agents/skills.",
}

// agentsReapPage is `agents reap -h`.
var agentsReapPage = helptext.Page{
	Usage: "claude-code agents reap [flags]",
	About: "Mark dispatch records still \"running\" whose process is gone as error, and drop stale " +
		"agent-inflight markers.",
}

func cmdAgents(args []string) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !slices.Contains([]string{"-h", "--help"}, args[0])) {
		return cmdAgentsList(args)
	}
	switch args[0] {
	case "sync":
		return cmdAgentsSync(args[1:])
	case "tree":
		return agent.RunTree(args[1:], paths.ConfigDir(), os.Stdout, os.Stderr)
	case "reap":
		return cmdAgentsReap(args[1:])
	case "status", "result", "tail", "cancel":
		return cmdAgentsJob(args[0], args[1:])
	case "send":
		return cmdAgentsSend(args[1:])
	case "-h", "--help", "help":
		agentsPage.Fprint(os.Stderr)
		return 0
	}
	fail("agents: unknown subcommand %q", args[0])
	agentsPage.Fprint(os.Stderr)
	return 1
}

// cmdAgentsReap marks dead-but-"running" dispatch records as error and drops stale agent-inflight markers.
func cmdAgentsReap(args []string) int {
	fs := helptext.NewFlagSet("agents reap", agentsReapPage)
	dryRun := fs.Bool("dry-run", false, "print what would change, write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return agent.Reap(*dryRun, os.Stdout, os.Stderr)
}

// cmdAgentsSync renders agents/ into state/projections, installs the copies into agent homes and links skills/.
func cmdAgentsSync(args []string) int {
	fs := helptext.NewFlagSet("agents sync", agentsSyncPage)
	vendor := fs.String("agent", "", "one target: "+strings.Join(crossgen.VendorNames(), "|")+" (default: all)")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	check := fs.Bool("check", false, "dry-run; exit 1 when the projection is stale")
	noPrune := fs.Bool("no-prune", false, "keep orphaned projections")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}
	return crossgen.Sync(crossgen.SyncOptions{DryRun: *dryRun, Check: *check, NoPrune: *noPrune, Vendor: *vendor, Out: os.Stdout})
}

// cmdAgentsList is bare `agents`: the local roster, or with --host the remote's own `agents` output.
func cmdAgentsList(args []string) int {
	fs := helptext.NewFlagSet("agents", agentsPage)
	host := fs.String("host", "", "list the agents on this ssh host instead (runs its agents --format)")
	remoteBin := fs.String("remote-bin", "", "remote claude-code path for --host (default $"+agent.RemoteBinEnv+", then claude-code)")
	format := fs.String("format", "text", "text, or json: per agent {name, binary, available, reason, caps, events}")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 || !slices.Contains([]string{formatText, formatJSON}, *format) {
		fs.Usage()
		return 2
	}
	if *host != "" {
		return listRemoteAgents(*host, agent.RemoteBin(*remoteBin), *format)
	}
	return listAgents(*format)
}

// listRemoteAgents runs `<bin> agents --format <format>` on host and prints its stdout; a failure shows ssh's stderr.
func listRemoteAgents(host, bin, format string) int {
	if strings.TrimSpace(bin) == "" || strings.ContainsAny(bin, "\r\n\x00") {
		return fail("agents: invalid remote binary %q", bin)
	}
	argv, err := tailscale.StreamArgs(host, "exec "+tailscale.QuotePath(bin)+" agents --format "+tailscale.QuoteSH(format))
	if err != nil {
		return fail("agents: %v", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fail("agents: %s: %v: %s", host, err, strings.TrimSpace(stderr.String()))
	}
	return 0
}

func listAgents(format string) int {
	type row struct {
		Name      string            `json:"name"`
		Binary    string            `json:"binary"`
		Available bool              `json:"available"`
		Reason    string            `json:"reason,omitempty"`
		Caps      map[string]string `json:"caps"`
		Events    []string          `json:"events"`
	}
	var rows []row
	for _, r := range agent.Runners() {
		mark, reason := "✓", ""
		if err := r.Available(); err != nil {
			mark, reason = "✗", err.Error()
		}
		caps := map[string]string{}
		var shown []string
		s, _ := agent.LookupSchema(r.Name())
		for _, f := range agent.CapFields(s.CLI.Caps) {
			caps[f.Name] = f.L.String()
			if f.L != agent.None && slices.Contains([]string{"model", "profile", "effort", "workdir", "session", "tools", "system-prompt",
				"append-system-prompt", "env", "output-schema"}, f.Name) {
				shown = append(shown, f.Name+":"+f.L.String())
			}
		}
		caps["cost"], caps["live"] = agent.None.String(), agent.None.String()
		if s.CLI.Cost {
			caps["cost"] = agent.Native.String()
		}
		if agent.HasLive(s.Name) {
			caps["live"] = agent.Native.String()
		}
		caps["background"], caps["remote"] = agent.Native.String(), agent.Native.String()
		binary := s.CLI.Binary
		if !s.External() {
			binary = s.Name
		}
		rows = append(rows, row{s.Name, binary, reason == "", reason, caps, s.CLI.Events})
		if format != formatJSON {
			fmt.Printf("%s %-9s %s %s\n", mark, r.Name(), strings.Join(shown, ","), reason)
		}
	}
	if format == formatJSON {
		_ = helptext.WriteJSON(os.Stdout, rows)
	}
	return 0
}

// printDispatchLog lists recent dispatches; the prompt preview is scrubbed of credential shapes.
func printDispatchLog(format string) int {
	recs := agent.RecentRecords(20)
	if format == formatJSON {
		_ = helptext.WriteJSON(os.Stdout, recs)
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("no dispatches logged yet")
		return 0
	}
	for _, r := range recs {
		preview := agent.Preview(r.Request.Prompt, 60)
		tools := ""
		if r.Request.Tools {
			tools = " tools"
		}
		agentCol := r.Agent
		if r.Fallback != "" {
			agentCol = r.Fallback + "→" + cmp.Or(r.RanAs, "?")
		}
		fmt.Printf("%-28s %-7s %-8s %-9s%s  %s\n", r.ID, r.Status, agentCol, r.Elapsed(), tools, preview)
		if r.FallbackReason != "" {
			fmt.Printf("%-28s   fallback: %s\n", "", r.FallbackReason)
		}
	}
	return 0
}

// batchOpts are the ask flags that apply to a whole --batch run.
type batchOpts struct {
	jobs            int
	failFast        bool
	agent, fallback string
	native          bool
}

// batchExclusive are the ask flags that make no sense on a batch of independent tasks.
var batchExclusive = []string{"retry", "job", "background", "session", "log", "attach", "rubric", "debug-file"}

// runAskBatch runs the JSONL tasks in src (a file, or "-" for stdin) on a bounded pool and streams one envelope per line.
func runAskBatch(fs *flag.FlagSet, resume bool, src string, o batchOpts, base agent.RunOptions) int {
	var bad error
	if resume || fs.NArg() > 0 {
		bad = errors.New("--batch takes tasks from the JSONL source, not a session id or a positional task")
	}
	for _, f := range batchExclusive {
		if flagExplicit(fs, f) {
			bad = fmt.Errorf("--batch cannot be combined with --%s", f)
		}
	}
	if flagExplicit(fs, "format") && fs.Lookup("format").Value.String() != formatJSON {
		bad = errors.New("--batch always prints JSON envelopes; drop --format or pass --format json")
	}
	if bad == nil && o.jobs < 1 {
		bad = fmt.Errorf("--jobs %d: need at least 1", o.jobs)
	}
	if bad != nil {
		return failAskCode("text", o.agent, bad, 2)
	}
	in := io.Reader(os.Stdin)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
		}
		defer f.Close()
		in = f
	}
	lines, err := agent.ReadTaskLines(in)
	if err != nil {
		return failAskCode("text", o.agent, fmt.Errorf("--batch: %w", err), 2)
	}
	if len(lines) == 0 {
		return failAskCode("text", o.agent, errors.New("--batch: no tasks in the input"), 2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE) // a closed stdout cancels
	defer stop()
	cwd, _ := os.Getwd() // resolved once here: a pool goroutine must never fall back to the process cwd
	timeoutSet := flagExplicit(fs, "timeout")
	seen := map[string]bool{}
	tasks := make([]agent.BatchTask, 0, len(lines))
	for _, l := range lines {
		t, err := batchTask(l, o, base, timeoutSet, cwd, seen)
		if err != nil {
			t.Err = err
			fail("ask: %v", err)
		}
		tasks = append(tasks, t)
	}
	id := agent.NewBatchID()
	ok := agent.RunBatch(ctx, tasks, agent.BatchOptions{ID: id, Jobs: o.jobs, FailFast: o.failFast, Out: os.Stdout, Log: os.Stderr})
	if !ok {
		fmt.Fprintf(os.Stderr, "ask: %s: not every task was ok\n", id)
		return 1
	}
	return 0
}

// batchTask validates one line like the ask flags and attaches the routing step; routing waits for Prepare.
func batchTask(l agent.ParsedLine, o batchOpts, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (agent.BatchTask, error) {
	t, err := agent.ParseBatchTask(l, o.agent, base, timeoutSet, cwd, seen)
	if err != nil {
		return t, err
	}
	req := t.Req
	req.Agent, req.Fallback, req.Native = t.Agent, o.fallback, o.native
	client := newClient()
	t.Prepare = func(ctx context.Context) (*agent.Prepared, error) {
		p, err := client.Prepare(ctx, req, nil)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", l.N, err)
		}
		return p, nil
	}
	return t, nil
}
