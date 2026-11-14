package ask

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"

	// Each vendor CLI registers its roster row and driver from its package init; this package links them all.
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
)

// askPage is `ask -h`; the task is the trailing args, or stdin when absent or "-".
var askPage = helptext.Page{
	Usage: "claude-code agents ask [flags] <task | ->",
	About: "Run one task headlessly on an agent and stream its answer; an agent that " +
		"fails falls back per --fallback. With no task, or \"-\", the task is read from stdin.",
	Hint: "Every dispatch is logged: `agents status` lists them, --retry <id> replays one.",
}

// Cmd is `agents ask` with no session id and the `agents ask` page.
func Cmd(args []string) int { return Run(args, "") }

// cmdTo is Cmd with the answer and envelope written to out instead of the process stdout.
func cmdTo(args []string, out io.Writer) int { return runTo(args, "", out) }

// retargetPage points fs's -h at `<agent> ask`: its own usage line, with --agent defaulting to the agent.
func retargetPage(fs *flag.FlagSet, name string) {
	p := askPage
	p.Usage = "claude-code " + name + " ask [flags] <task | ->"
	fs.Lookup("agent").DefValue = name
	fs.Usage = func() {
		p.Flags = fs
		p.Fprint(fs.Output())
	}
}

// askRun is one ask invocation: its flags plus what the setup phases resolve.
type askRun struct {
	ctx         context.Context // the run's values: the phases that export lay theirs over a snapshot of the env
	f           *askFlags
	fs          *flag.FlagSet
	args        []string
	out         io.Writer // where the answer and envelope go: stdout, or a review part's buffer
	ref         string    // the raw selector Prepare resolves; f.name becomes the agent so envelopes publish it
	ag          string
	persona     *agent.Schema
	defs        map[string]*agent.Schema
	ov          agentconfig.Overlay // --overlay, else $COWORKER_OVERLAY: routing keys fold under typed flags
	recCoworker string              // the confinement a --retry/--job record ran on
	confined    bool                // the confinement filled the lead: resolveLead treats it as typed
	schema      string
	jobRec      *agent.Record
	prompt      string
	state       *askState
	perms       agent.Permissions
	lead        string // profile a routed run leads with (--profile session, --provider, or --profile with --fallback)
	auto        string // the workspace databricks:auto picked for this run; "" when the confinement was not an auto pick
}

// Run is `agents ask`; verb names the `<verb> ask` page -h prints.
func Run(args []string, verb string) int { return runTo(args, verb, os.Stdout) }

// runTo is Run writing the answer and envelope to out.
func runTo(args []string, verb string, out io.Writer) int {
	fs, f := askFlagSet()
	if verb != "" {
		retargetPage(fs, verb)
	}
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if !helptext.Explicit(fs, "fallback") {
		f.fallback = router.DefaultFallback()
	}
	if remote.IsSelf(f.host) { // --host naming this machine runs here
		f.host = ""
	}
	r := &askRun{ctx: context.Background(), f: f, fs: fs, args: args, out: out}
	for _, phase := range []func() (int, bool){r.config, r.prelude, r.selectAgent, r.parsePerms, r.dispatchMode, r.resolveRecord, r.checkArm, r.resolveLead, r.parsePerms, r.export} {
		if code, done := phase(); done {
			return code
		}
	}
	return r.execute()
}

func (r *askRun) fail(err error, code int) (int, bool) {
	return failAskCode(r.f.format, r.f.name, err, code), true
}

// config loads the overlay once and folds its routing keys under the typed flags; a bad value is a usage error.
func (r *askRun) config() (int, bool) {
	busy := ""
	if r.stdinTaken() {
		busy = "stdin is the overlay, so give the task as an argument"
	}
	ov, err := LoadOverlay(r.ctx, r.f.overlay, busy)
	if err != nil {
		return r.fail(err, 2)
	}
	if remote.IsSelf(ov.Host) { // an overlay host naming this machine runs here, like --host
		ov.Host = ""
	}
	r.ov = ov
	if err := r.remoteWithHost(); err != nil {
		return r.fail(err, 2)
	}
	ov.Fold(r.fs)
	return 0, false
}

// LoadOverlay loads --overlay value, else $COWORKER_OVERLAY; a "-" source fails with busy when that is set.
func LoadOverlay(ctx context.Context, value, busy string) (agentconfig.Overlay, error) {
	if busy != "" && cmp.Or(value, proc.Value(ctx, agentconfig.OverlayVar)) == "-" {
		from := "--overlay -"
		if value == "" {
			from = agentconfig.OverlayVar + "=-"
		}
		return agentconfig.Overlay{}, errors.New(from + ": " + busy)
	}
	return agentconfig.LoadOverlayContext(ctx, agentconfig.DispatchOverlayContext(ctx, value), os.Stdin)
}

// remoteWithHost refuses an overlay remote on a run that goes to --host (typed, or restored from a record).
func (r *askRun) remoteWithHost() error {
	if r.ov.Remote != "" && r.f.host != "" {
		return fmt.Errorf("overlay %w", agentconfig.ErrRemoteWithHost)
	}
	return nil
}

// export runs once every usage check has passed: the overlay's egress host, and the document for --job relaunches.
func (r *askRun) export() (int, bool) {
	err := r.remoteWithHost()
	if err == nil {
		r.ctx, err = overlayEgress(r.ctx, r.ov.Remote)
	}
	if err == nil {
		r.ctx, err = carryOverlay(r.ctx, r.ov)
	}
	if err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// stdinTaken reports the task, a batch or a framed prompt already reading stdin.
func (r *askRun) stdinTaken() bool {
	f := r.f
	return f.stdinFramed || f.batchSrc == "-" || (taskFromStdin(r.fs.Args()) && f.retry == "" && f.job == "" && !f.live)
}

// taskFromStdin reports no task positional, or a lone "-".
func taskFromStdin(task []string) bool {
	return len(task) == 0 || (len(task) == 1 && task[0] == "-")
}

// prelude turns on --overlay-debug and validates the flags.
func (r *askRun) prelude() (int, bool) {
	f := r.f
	if code := askEnableOverlayDebug(f.overlayDebug); code != 0 {
		return code, true
	}
	var err error
	if f.pack, err = askPack(r.fs, f); err != nil {
		return r.fail(err, 2)
	}
	f.tools = f.tools || f.readOnly // read-only tools are still tools
	if err := askCheckFlags(f.env, f.keepEnv, f.format, f.fallback, f.background, f.job, f.tools, f.noTools); err != nil {
		return r.fail(err, 2)
	}
	if r.schema, err = resolveOutputSchema(f.outputSchema); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// selectAgent resolves the --agent selector and checks the flag combinations it enables.
func (r *askRun) selectAgent() (int, bool) {
	f := r.f
	var err error
	if r.defs, err = parseAgentDefs(string(f.agents), f.host); err != nil {
		return r.fail(err, 2)
	}
	r.ref = f.name
	if r.ag, r.persona, err = agent.SelectAgent(r.ref, r.defs); err != nil {
		return r.fail(err, 2)
	}
	f.name = r.ag
	if err := agent.ValidateAs(f.as, f.name); err != nil { // against the resolved agent: a claude persona passes
		return r.fail(err, 2)
	}
	if err := askCheckCombos(r.fs, f.name, f.live, r.persona != nil, f.host, f.background, f.job, f.batchSrc, len(f.env) > 0); err != nil {
		return r.fail(err, 2)
	}
	if err := overlayLocal(r.ov, f.live, f.host); err != nil {
		return r.fail(err, 2)
	}
	if f.host != "" && f.leadProvider != "" { // the remote ask carries --profile, never a --provider lead
		return r.fail(errors.New("--host cannot be combined with --provider: name the far side's profile with --profile"), 2)
	}
	return 0, false
}

// checkArm applies the overlay's confinement for the agent and refuses flags leaving it (exit 2; preflight: 1).
func (r *askRun) checkArm() (int, bool) {
	f := r.f
	value, err := recordConfinement(r.ov.Confinement(), r.recCoworker)
	if err != nil {
		return r.fail(err, 2)
	}
	agentName, batchLines := confineAgent(r.ov, f, r.ag)
	picks := armAutoPick(r.ctx, value)
	// applied here, not in export: CheckArm, ConfineLead and resolveLead read the confinement from the run's values
	ctx, err := ApplyCoworker(r.ctx, value, f.provider, agentName, f.model, f.fallback)
	if err != nil {
		return r.fail(err, ApplyCode(err))
	}
	r.ctx = ctx
	if picks || r.jobAutoArm() {
		r.auto = router.ArmProfileContext(r.ctx)
	}
	if err := cmp.Or(checkNative(r.ctx, f.native), CheckArm(r.ctx, r.ag, f.provider, f.leadProvider, f.model, f.direct)); err != nil {
		return r.fail(err, 2)
	}
	if !batchLines {
		r.confined = confineLead(r.ctx, r.ag, f) || r.confined
	}
	return 0, false
}

// confineAgent is the agent the confinement is applied for ("" when each batch line confines its own), and that flag.
func confineAgent(ov agentconfig.Overlay, f *askFlags, ag string) (string, bool) {
	if f.batchSrc != "" && ov.Provider != router.RouteDatabricks { // confineLine does each line's agent
		return "", true
	}
	return ag, false
}

// ApplyCode is the exit code for an ApplyCoworker error: a preflight refusal is 1, the rest usage errors.
func ApplyCode(err error) int {
	if _, ok := errors.AsType[*agent.PreflightError](err); ok {
		return 1
	}
	return 2
}

// confineLead narrows --provider and the lead to the overlay's confinement, reporting whether either changed.
func confineLead(ctx context.Context, ag string, f *askFlags) bool {
	profile, provider := router.ConfineLeadContext(ctx, ag, f.provider, f.leadProvider, f.model)
	changed := profile != f.provider || provider != f.leadProvider
	f.provider, f.leadProvider = profile, provider
	return changed
}

// recordConfinement is the overlay's confinement, else a replayed record's: "default" is none, a retired value errs.
func recordConfinement(own, rec string) (string, error) {
	if own != "" || rec == "" || rec == "default" {
		return own, nil
	}
	if _, _, err := router.ParseCoworker(rec); err != nil {
		return "", fmt.Errorf("the replayed record ran under route %q, which no longer exists: pass --overlay with a provider", rec)
	}
	return rec, nil
}

// parsePerms validates --allow/--ask/--deny/--mode into r.perms; it reruns after a --retry record fills them.
func (r *askRun) parsePerms() (int, bool) {
	f := r.f
	p, err := agent.ParsePermissions(f.permMode, f.permAllow, f.permAsk, f.permDeny)
	if err != nil {
		return r.fail(err, 2)
	}
	r.perms = p
	return 0, false
}

// dispatchMode handles --batch, which ends the run before a single task is built.
func (r *askRun) dispatchMode() (int, bool) {
	f := r.f
	if f.batchSrc != "" { // --batch picks once from the base flags; a single task checks after its record
		if code, done := r.checkArm(); done {
			return code, true
		}
		return r.runBatch()
	}
	if err := batchOnlyFlags(r.fs); err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// runBatch runs --batch to completion; every path ends the run.
func (r *askRun) runBatch() (int, bool) {
	f := r.f
	if code, done := r.resolveLead(); done { // validates --provider/--profile and sets r.lead for batchBase
		return code, true
	}
	o := batchOpts{Jobs: f.jobs, FailFast: f.failFast, Agent: r.ref, Fallback: f.fallback, Native: f.native, Auto: r.auto, Identity: f.as}
	ov, err := resolveOverlay(r.ov, f.env, f.keepEnv, false)
	if err != nil {
		return r.fail(err, 2)
	}
	if code, done := r.export(); done {
		return code, true
	}
	return runAskBatch(r.ctx, r.fs, f.batchSrc, o, r.batchBase().ApplyOverlay(r.ctx, ov)), true
}

// batchOnlyFlags refuses the first --batch-only flag typed without --batch.
func batchOnlyFlags(fs *flag.FlagSet) error {
	for _, name := range []string{"jobs", "fail-fast"} {
		if helptext.Explicit(fs, name) {
			return fmt.Errorf("--%s needs --batch", name)
		}
	}
	return nil
}

func (r *askRun) batchBase() agent.RunOptions {
	f := r.f
	_, values := agent.RunValues(r.ctx, nil)
	return agent.RunOptions{Model: f.model, Provider: f.provider, Lead: r.lead, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools,
		Timeout: f.timeout, KeepEnv: f.keepEnv, Autonomous: f.autonomous, Fleet: f.fleet, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: f.env, OutputSchema: r.schema, Agents: r.defs, Permissions: r.perms, Values: values}
}

// resolveRecord reads the task and applies a --job or --retry record over the flags.
func (r *askRun) resolveRecord() (int, bool) {
	f := r.f
	r.prompt = strings.Join(r.fs.Args(), " ")
	r.state = r.askState()
	if rc, done := r.applyRecord(); done {
		return rc, true
	}
	var err error
	f.model = cmp.Or(f.model, agent.SessionModel(f.session)) // a resumed session keeps its model; a typed --model wins
	if err := recordTools(f); err != nil {
		return r.fail(err, 2)
	}
	if r.ref, r.ag, err = askReselect(r.ref, r.ag, &f.name, &r.persona, r.defs); err != nil {
		return r.fail(err, 2)
	}
	if err := resolveAskHost(r.ov, f); err != nil {
		return r.fail(err, 2)
	}
	if f.outputSchema != "" { // a --retry/--job record may carry an already-resolved schema: re-resolve the flag
		if r.schema, err = resolveOutputSchema(f.outputSchema); err != nil {
			return r.fail(err, 2)
		}
	}
	return 0, false
}

// recordTools re-folds --read-only into tools after a --job/--retry record, refusing the tools pair it completes.
func recordTools(f *askFlags) error {
	f.tools = f.tools || f.readOnly
	if err := askExclusive(f.background, f.job, f.tools, f.noTools); err != nil {
		return fmt.Errorf("%w: the replayed record sets one; type it off (--tools=false --read-only=false or --no-tools=false)", err)
	}
	return nil
}

// applyRecord applies a --job record, else a --retry record under the overlay's fold.
func (r *askRun) applyRecord() (int, bool) {
	f := r.f
	var rc int
	var done bool
	if r.jobRec, rc, done = loadAskJob(f.job, f.format, r.state, &r.prompt); done {
		return rc, true
	}
	if r.jobRec != nil {
		return 0, false // a --job record already holds the fold
	}
	if rc, done := loadAskRetry(r.ctx, r.fs, r.args, f.retry, f.format, r.state, &r.prompt); done {
		return rc, true
	}
	if f.retry != "" && !helptext.Explicit(r.fs, "session") {
		rec, err := agent.LoadRecord(f.retry)
		if err != nil {
			return r.fail(err, 1)
		}
		if rec.RanAs == "" || rec.RanAs == rec.Agent {
			f.session = cmp.Or(rec.SessionID, f.session)
		}
	}
	r.ov.Fold(r.fs) // the overlay beats a --retry record
	return 0, false
}

// resolveAskHost re-checks the overlay against --host and resolves it; a --retry/--job record may carry a --host.
func resolveAskHost(ov agentconfig.Overlay, f *askFlags) error {
	if err := overlayLocal(ov, f.live, f.host); err != nil {
		return err
	}
	var err error
	if f.host, err = remote.ResolveAuto(f.host); err != nil { // here: a --retry re-parse may have typed auto again
		return fmt.Errorf("%s %w", remote.Flag, err)
	}
	if remote.IsSelf(f.host) { // --host naming this machine runs here
		f.host = ""
	}
	return nil
}

func (r *askRun) askState() *askState {
	f := r.f
	return &askState{Name: &f.name, Provider: &f.provider, Model: &f.model, Effort: &f.effort,
		Workdir: &f.workdir, Session: &f.session, Tools: &f.tools, ReadOnly: &f.readOnly, NoTools: &f.noTools,
		Timeout: &f.timeout, Attach: (*[]string)(&f.attach), KeepEnv: &f.keepEnv, Autonomous: &f.autonomous,
		Fleet: &f.fleet, EnableMemory: &f.enableMemory, Direct: &f.direct, NoPersist: &f.noPersist, SafeMode: &f.safeMode,
		DebugFile: &f.debugFile, SystemPrompt: &f.systemPrompt, AppendSystemPrompt: &f.appendSystemPrompt,
		OutputSchema: &f.outputSchema, Persona: &r.persona, Host: &f.host, Fallback: &f.fallback,
		PermMode: &f.permMode, PermAllow: &f.permAllow, PermAsk: &f.permAsk, PermDeny: &f.permDeny,
		Native: &f.native, Pack: &f.pack, Coworker: &r.recCoworker, rubric: &f.rubric, env: &f.env, lead: &r.lead}
}

// execute starts a live session or runs the one task.
func (r *askRun) execute() int {
	f := r.f
	if f.live {
		return startLive(r.ctx, r.state, r.perms, f.name, f.as, f.format)
	}
	if r.jobRec != nil && r.jobRec.Live {
		return agent.RunHolder(r.jobRec, newClient().Launcher)
	}
	framed, prompt, err := r.readPrompt()
	if err != nil {
		return failAsk(f.format, f.name, err)
	}
	if strings.TrimSpace(prompt) == "" {
		r.fs.Usage()
		printAskFailure(f.format, f.name, errors.New("no task: give a prompt, or \"-\" to read stdin"))
		return 2
	}
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // a kill finalizes the record
	defer stop()
	if framed != nil { // the remote end of `ask --host`: stdin closing means the local side cancelled or vanished
		go func() { _, _ = io.Copy(io.Discard, framed); stop() }()
	}
	f.timeout = askTimeout(r.fs, f.timeout, f.retry, f.job, f.tools)
	warnAskMemory(r.ag, f.enableMemory)
	f.debugFile = absDebugFile(f.debugFile)
	req, run := r.request(prompt), askRunner(nil)
	if a := r.armRun(); a != nil {
		req, run = a.probe(req), a.run
	}
	return askPrepareAndRun(ctx, r.fs, req, r.jobRec, r.ov, f.env, f.keepEnv, f.as, f.format, f.name, f.background, f.native, run, r.out)
}

// readPrompt reads the task; framed (stdin) is set only for the remote end of `ask --host`.
func (r *askRun) readPrompt() (*bufio.Reader, string, error) {
	if r.f.stdinFramed {
		return readFramedPrompt(os.Stdin)
	}
	prompt, err := resolveAskPrompt(r.prompt)
	return nil, prompt, err
}

// request is batchBase plus the single-task fields.
func (r *askRun) request(prompt string) agent.RunOptions {
	f := r.f
	o := r.batchBase()
	o.Prompt, o.SessionID, o.Attach, o.DebugFile, o.Pack, o.Agent = prompt, f.session, []string(f.attach), f.debugFile, f.pack, r.ref
	o.Persona, o.Fallback, o.Native, o.Host, o.Rubric, o.Background = r.persona, f.fallback, f.native, f.host, f.rubric, f.background
	o.AutoArm = r.auto != ""
	return o
}

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches it or runs it (run, else once) and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, doc agentconfig.Overlay,
	env, keepEnv []string, as, format, name string, background, native bool, run askRunner, out io.Writer) int {
	ov, err := resolveOverlay(doc, env, keepEnv, background)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	req = req.ApplyOverlay(ctx, ov)
	if err := agent.CheckCapabilities(name, req); err != nil {
		return failAskCode(format, name, err, 2)
	}
	cl := newClient()
	ctx = providers.WithIdentity(ctx, cmp.Or(as, providers.IdentityHeadless)) // headless: a paired row tries its SP first
	prepared, err := cl.Prepare(ctx, req, jobRec)
	if err != nil {
		return failAskCode(format, name, err, prepareExitCode(err))
	}
	saveErr := startPrepared(cl, prepared, background)
	if background {
		if saveErr != nil { // the --job child loads this record: without it the job could never start
			return failAsk(format, name, fmt.Errorf("save dispatch record: %w", saveErr))
		}
		return askSpawn(ctx, fs, prepared, native, format, name)
	}
	if run != nil {
		return run(ctx, cl, prepared, req)
	}
	return runAskAndReport(ctx, cl, prepared, jobRec != nil, format, out)
}

// startPrepared prints p's preflight lines and saves its record; a failed save is warned about unless background.
func startPrepared(cl *agent.Client, p *agent.Prepared, background bool) error {
	for _, l := range p.Chain.Preflight.Lines() {
		fmt.Fprintln(os.Stderr, "preflight:", l)
	}
	cl.Notify = quietPrinted(cl.Notify, p.Chain.Preflight)
	err := p.Record.Save()
	if err != nil && !background {
		fmt.Fprintf(os.Stderr, "ask: warning: dispatch record not saved, --retry may not find it: %v\n", err)
	}
	return err
}

// quietPrinted drops the skip entries notify would repeat after pf's preflight: lines; other messages pass.
func quietPrinted(notify func(string), pf *agent.Preflight) func(string) {
	if notify == nil || pf == nil || len(pf.Failed) == 0 {
		return notify
	}
	return func(s string) {
		rest, ok := strings.CutPrefix(s, agent.SkippedLead)
		if !ok {
			notify(s)
			return
		}
		kept := slices.DeleteFunc(strings.Split(rest, "; "), func(e string) bool {
			return slices.ContainsFunc(pf.Failed, func(f agent.Finding) bool { return strings.HasSuffix(e, ": preflight "+f.String()) })
		})
		if len(kept) > 0 {
			notify(agent.SkippedLead + strings.Join(kept, "; "))
		}
	}
}

func warnAskMemory(ag string, enableMemory bool) {
	if s, ok := agent.LookupSchema(ag); enableMemory && ok && s.CLI.Caps.Memory == agent.None {
		fmt.Fprintf(os.Stderr, "ask: --enable-memory has no effect on the %s agent (copilot only)\n", ag)
	}
}

// routesItself: a record (no route flag typed), the --host end, or no profile/provider leaves the lead to Prepare.
func (r *askRun) routesItself() bool {
	typed := helptext.Explicit(r.fs, "profile") || helptext.Explicit(r.fs, "provider") || helptext.Explicit(r.fs, "fallback") || r.confined
	return !typed || r.f.host != "" || (r.f.provider == "" && r.f.leadProvider == "")
}

// resolveLead turns route flags typed on this command line into a routed lead (agent.LeadFor); a pin keeps --profile.
func (r *askRun) resolveLead() (int, bool) {
	lead, err := narrowLead(r.ag, r.f, !r.routesItself())
	if err != nil {
		return r.fail(err, 2)
	}
	r.lead = cmp.Or(lead, r.lead)
	return 0, false
}

// narrowLead routes f's --provider/--profile into a lead and f.provider when routed; "" and no change otherwise.
func narrowLead(ag string, f *askFlags, routed bool) (string, error) {
	if !routed {
		return "", nil
	}
	fallback := f.fallback
	if f.session != "" { // a resumed session runs one hop
		fallback = router.FallbackNone
	}
	q := agent.RouteReq{Agent: ag, Model: f.model, Profile: f.provider, Provider: f.leadProvider, Fallback: fallback}
	lead, provider, err := routedLead(q, ag == "claude" || f.live)
	if err != nil {
		return "", err
	}
	f.provider = provider
	return lead, nil
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

// askSpawn detaches a prepared dispatch as a background job; its child starts from the run values it was prepared with.
func askSpawn(ctx context.Context, fs *flag.FlagSet, prepared *agent.Prepared, native bool, format, name string) int {
	if prepared.Opts.Values != nil {
		ctx = proc.WithValues(ctx, prepared.Opts.Values)
	}
	if err := agent.SpawnContext(ctx, &prepared.Record, backgroundExtraFlags(fs, prepared.Opts.Fallback, native)); err != nil {
		return failAsk(format, name, failSpawn(&prepared.Record, err))
	}
	if err := reportBackgroundStart(format, prepared.Record); err != nil {
		return helptext.Fail("%s: job %s started; write job id: %v", name, prepared.Record.ID, err)
	}
	return 0
}

// stdoutSink is the sink behind stdout for format: streamed text, nothing (json prints one envelope), or NDJSON.
func stdoutSink(format, dispatchID string) wire.Sink { return sinkTo(os.Stdout, format, dispatchID) }

// sinkTo is stdoutSink writing to out.
func sinkTo(out io.Writer, format, dispatchID string) wire.Sink {
	switch format {
	case helptext.JSON:
		return wire.Discard
	case agent.FormatStreamJSON:
		return wire.NewJSONLSink(out, dispatchID)
	}
	return wire.NewTextSink(out)
}

// backgroundExtraFlags carries explicit routing and identity flags over to the spawned --job child.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "as":
			extra = append(extra, "--as", f.Value.String())
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
func reportBackgroundStart(format string, rec agent.Record) error {
	if format == helptext.JSON {
		return agent.WriteJSONLine(os.Stdout, struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
	}
	_, err := fmt.Printf("job %s started — claude-code agents status|result|cancel %s\n", rec.ID, rec.ID)
	return err
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	helptext.Fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json; a lost write is noted on stderr.
func printAskFailure(format, agentName string, err error) {
	var werr error
	switch format {
	case helptext.JSON:
		werr = agent.WriteJSONLine(os.Stdout, wire.FailureEnvelope(agentName, err))
	case agent.FormatStreamJSON:
		werr = stdoutSink(format, "").Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "ask: failure envelope not written to stdout: %v\n", werr)
	}
}
