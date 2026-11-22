package ask

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
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
	Hint: "A task starting with - must follow `--`. Dispatches log: `agents status`.",
}

// Cmd is `agents ask` with no session id and the `agents ask` page.
func Cmd(args []string) int { return Run(args, "") }

// cmdTo is Cmd with the answer and envelope written to out instead of the process stdout.
func cmdTo(args []string, out io.Writer) int { return runCtx(context.Background(), args, "", out) }

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
	pos         []string  // the task words: what is left once every flag, before or after them, is parsed
	out         io.Writer // where the answer and envelope go: stdout, or a review part's buffer
	ref         string    // the raw selector Prepare resolves; f.name becomes the agent so envelopes publish it
	ag          string
	persona     *agent.Schema
	defs        map[string]*agent.Schema
	ov          agentconfig.Overlay // --overlay, else $COWORKER_OVERLAY: routing keys fold under typed flags
	ovSrc       ovSource            // where ov came from: its route is a fence only when inherited
	flagRoute   bool                // --profile / --provider name a route: it replaces every lower layer
	recCoworker string              // the confinement a --retry/--job record ran on
	confined    bool                // the confinement filled the lead: resolveLead treats it as typed
	schema      string
	jobRec      *agent.Record
	prompt      string
	state       *askState
	perms       permission.Permissions
	typed       string // --profile as typed, kept for the --dry-run argv: resolveLead clears f.profile
	lead        string // profile a routed run leads with (--profile session, --provider, or --profile with --fallback)
	swap        bool   // --fallback was typed and allows another model: a typed model may then be swapped
	auto        string // the workspace databricks:auto picked for this run; "" when the confinement was not an auto pick
}

// Run is `agents ask`; verb names the `<verb> ask` page -h prints.
func Run(args []string, verb string) int {
	return runCtx(context.Background(), args, verb, os.Stdout)
}

// parseAskArgs parses flags in any position; an unknown flag-looking word exits 2, and "--" ends the flags.
func parseAskArgs(fs *flag.FlagSet, args []string) (pos []string, code int, done bool) {
	pos, err := helptext.ParseArgs(fs, args)
	if err == nil {
		return pos, 0, false
	}
	if strings.Contains(err.Error(), "not defined") {
		fmt.Fprintln(os.Stderr, "claude-code: a task that starts with - must follow `--`, e.g. agents ask -- \"-1 is wrong\"")
	}
	return nil, helptext.ExitCode(err), true
}

// Waves runs `ask --waves <plan>` on the raw args; the cli sets it.
var Waves = func([]string) int { return 2 }

// Reap starts the silent cleanup of a real ask and returns its join; the cli sets it.
var Reap = func() func() { return func() {} }

// runCtx is Run under ctx, whose run values (a parent dispatch id) the new dispatch inherits.
func runCtx(ctx context.Context, args []string, verb string, out io.Writer) (exit int) {
	fs, f, args, pos, code, done := parseAsk(verb, args)
	if done {
		return code
	}
	if err := checkAncestry(ctx); err != nil {
		return failAsk(f.format, f.name, err)
	}
	if !helptext.Explicit(fs, "fallback") {
		f.fallback = router.DefaultFallback()
		if !slices.Contains(router.FallbackModes, f.fallback) {
			err := fmt.Errorf("%s=%q unknown (%s)", router.EnvFallback, f.fallback, strings.Join(router.FallbackModes, "|"))
			return failAskCode(f.format, f.name, err, 2)
		}
	}
	if remote.IsSelf(f.host) { // --host naming this machine runs here
		f.host = ""
	}
	if f.format == helptext.Text {
		masked := maskedOut(out)
		defer func() { exit = closeAskOutput(masked, exit) }()
		out = masked
	}
	if f.dryRun { // one peek mark for every pick a dry-run makes, the workspace's included
		ctx = agent.PeekContext(ctx)
	} else {
		defer Reap()() // flags and any spec, stdin included, are parsed, so dryness is final; a dry run reaps nothing
	}
	r := &askRun{ctx: ctx, f: f, fs: fs, args: args, pos: pos, out: out}
	markTyped(fs, f) // before dispatchMode: a batch builds its base request there
	for _, phase := range []func() (int, bool){r.config, r.prelude, r.selectAgent, r.parsePerms, r.carryMode, r.dispatchMode, r.resolveRecord, r.checkArm, r.resolveLead, r.parsePerms, r.export} {
		if code, done := phase(); done {
			return code
		}
	}
	return r.execute()
}

var checkAncestry = agent.CheckAncestorDispatch

// markTyped records a --model typed on this command line; auto asks for rotation and pins nothing.
func markTyped(fs *flag.FlagSet, f *askFlags) {
	if helptext.Explicit(fs, "model") {
		auto := agent.IsAuto(f.model)
		f.modelTyped = f.model != "" && !auto
	}
	f.timeoutTyped = f.timeoutTyped || helptext.Explicit(fs, "timeout")
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
	if err := takeRoute(r); err != nil {
		return r.fail(err, 2)
	}
	r.ov.Fold(r.fs)
	return 0, false
}

// ovSource is where the loaded overlay came from; only an inherited one is a fence, the others are sources.
type ovSource int

const (
	srcVault ovSource = iota // $COWORKER_OVERLAY in the vault env, or a --spec template's overlay
	srcCmd                   // --overlay on this command line
	srcFence                 // $COWORKER_OVERLAY inherited from a parent dispatch
)

// takeRoute settles the route unit whole: --overlay and the flags must agree; flags beat a vault or template overlay.
func takeRoute(r *askRun) error {
	f := r.f
	switch {
	case f.overlay != "" && f.spec == "":
		r.ovSrc = srcCmd
	case f.overlay == "" && proc.Value(r.ctx, route.OverlayVar) != "":
		r.ovSrc = srcFence
	}
	fr := flagRoute(f)
	r.flagRoute = !fr.IsZero()
	if r.ovSrc == srcCmd {
		return flagConflict(r.fs, f, fr, r.ov)
	}
	if r.ovSrc == srcVault && r.flagRoute {
		r.ov = r.ov.WithoutRoute()
	}
	return nil
}

// confinementValue is the provider[:profile] the run is fenced to: the overlay's, else the replayed record's.
// Typed route flags name the route themselves, so a record's never mixes in.
func confinementValue(r *askRun) (string, error) {
	rec := r.recCoworker
	if r.flagRoute {
		rec = ""
	}
	return recordConfinement(r.ov.Confinement(), rec)
}

// LoadOverlay loads --overlay value, else $COWORKER_OVERLAY; a "-" source fails with busy when that is set.
func LoadOverlay(ctx context.Context, value, busy string) (agentconfig.Overlay, error) {
	if busy != "" && cmp.Or(value, proc.Value(ctx, route.OverlayVar)) == "-" {
		from := "--overlay -"
		if value == "" {
			from = route.OverlayVar + "=-"
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
	return f.stdinFramed || f.batchSrc == "-" || f.spec == "-" || (taskFromStdin(r.pos) && f.retry == "" && f.job == "" && !f.live)
}

// taskFromStdin reports no task positional, or a lone "-".
func taskFromStdin(task []string) bool {
	return len(task) == 0 || (len(task) == 1 && task[0] == "-")
}

// prelude validates the flags.
func (r *askRun) prelude() (int, bool) {
	f := r.f
	var err error
	if f.pack, err = askPack(r.fs, f); err != nil {
		return r.fail(err, 2)
	}
	f.tools = f.tools || f.readOnly // read-only tools are still tools
	if err := cmp.Or(dryCombos(f), askCheckFlags(f.env, f.keepEnv, f.format, f.fallback, f.background, f.job, f.tools, f.noTools)); err != nil {
		return r.fail(err, 2)
	}
	if f.save != "" && (f.background || f.live || f.batchSrc != "") {
		return r.fail(errors.New("--save needs a foreground run: not with --background, --live or --batch"), 2)
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
	if f.codemode, err = validateCodemode(f.codemode, f.name, f.tools); err != nil {
		return r.fail(err, 2)
	}
	if err := askCheckCombos(r.fs, f.name, f.live, r.persona != nil, f.host, f.background, f.job, f.batchSrc, len(f.env) > 0); err != nil {
		return r.fail(err, 2)
	}
	if err := overlayLocal(r.ov, f.live, f.host); err != nil {
		return r.fail(err, 2)
	}
	if err := r.ov.CheckCloudAgent(f.name); err != nil {
		return r.fail(err, 2)
	}
	if f.host != "" && f.provider != "" { // the remote ask carries --profile, never a --provider lead
		return r.fail(errors.New("--host cannot be combined with --provider: name the far side's profile with --profile"), 2)
	}
	return 0, false
}

// checkArm applies the overlay's confinement for the agent and refuses flags leaving it (exit 2; preflight: 1).
func (r *askRun) checkArm() (int, bool) {
	f := r.f
	value, err := confinementValue(r)
	if err != nil {
		return r.fail(err, 2)
	}
	value = r.poolValue(value)
	agentName, batchLines := confineAgent(r.ov, f, r.ag)
	picks := armAutoPick(r.ctx, value)
	fallback := f.fallback
	if picks && !r.swap { // an env default never licenses swapping a typed model
		fallback = router.FallbackNone
		if typedModel(f.model) { // the chain and a 429 retry both keep the typed model
			f.fallback = fallback
		}
	}
	// applied here, not in export: CheckArm, ConfineLead and resolveLead read the confinement from the run's values
	ctx, err := ApplyCoworker(r.ctx, value, f.profile, agentName, f.model, fallback)
	if err != nil {
		return refuse(r, err, fallback)
	}
	r.ctx = ctx
	r.markAuto(picks)
	if err := cmp.Or(checkNative(r.ctx, f.native), CheckArm(r.ctx, r.ag, f.profile, f.provider, f.model, f.direct)); err != nil {
		return r.fail(err, 2)
	}
	if !batchLines {
		r.confined = confineLead(r.ctx, r.ag, f) || r.confined
	}
	return 0, false
}

// markAuto records an auto-picked workspace; a --job child (explicit profile) regains the parent's failover grant.
func (r *askRun) markAuto(picks bool) {
	if !picks && !r.jobAutoArm() {
		return
	}
	r.auto = router.ArmProfileContext(r.ctx)
	if !picks {
		r.ctx = agent.WithAutoFailover(r.ctx, router.RouteDatabricks)
	}
}

// refuse fails a route refusal; a well-formed ask (exit 1, not --dry-run) also leaves its failed record.
func refuse(r *askRun, err error, fallback string) (int, bool) {
	code := ApplyCode(err)
	if code == 1 && !r.f.dryRun {
		recordRefusal(r, err, fallback)
	}
	return r.fail(err, code)
}

// recordRefusal concludes a job's record, else records a single task with a prompt; a --batch base refusal has none.
func recordRefusal(r *askRun, err error, fallback string) {
	f := r.f
	if r.jobRec != nil {
		agent.RecordRefusal(r.ag, r.jobRec.Request, r.jobRec, err)
		return
	}
	if f.batchSrc != "" || f.live {
		return
	}
	_, prompt, perr := r.readPrompt()
	if perr != nil || strings.TrimSpace(prompt) == "" {
		return
	}
	f.timeout = askTimeout(r.fs, f.timeout, f.retry, f.job, f.tools)
	req := r.request(prompt)
	req.Fallback = fallback
	agent.RecordRefusal(r.ag, req, nil, err)
}

// poolValue is value, or the pool confinement for --provider databricks alone, a pool pin, or a model the pin lacks.
func (r *askRun) poolValue(value string) string {
	f := r.f
	r.swap = helptext.Explicit(r.fs, "fallback") && f.fallback != router.FallbackNone
	auto := route.Join(router.RouteDatabricks, route.AutoArm)
	f.native = ownLoginNative(r.ag, f, value)
	switch {
	case value != "" || f.profile != "" || f.host != "" || r.jobRec != nil || f.native:
	case f.provider == router.RouteDatabricks:
		f.provider = ""
		return auto
	case pinMoves(r):
		return auto
	case poolPinned(r):
		return storedWorkspace(r, auto)
	}
	return value
}

// poolPinned reports an unled, unbatched run on a stored pool pin, including held runs.
func poolPinned(r *askRun) bool {
	f := r.f
	return f.provider == "" && f.batchSrc == "" && agent.PoolPicks(r.ag, f.model, false)
}

// storedWorkspace is databricks:<workspace> as a label resolves the stored pin, else the pool pick.
func storedWorkspace(r *askRun, pool string) string {
	reg, err := providers.Load()
	if err != nil {
		return pool
	}
	q, _, err := agent.RunQuery(r.ctx, reg, agent.ModelQuery{Agent: r.ag}, "")
	if err != nil || q.Profile == "" {
		return pool
	}
	return route.Join(router.RouteDatabricks, q.Profile)
}

// pinMoves reports an unled, unbatched run off its pin: a typed model the pin lacks.
func pinMoves(r *askRun) bool {
	f := r.f
	if f.provider != "" || f.batchSrc != "" {
		return false
	}
	return !r.swap && typedModel(f.model) && r.pinLacks(f.model)
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
	profile, provider := router.ConfineLeadContext(ctx, ag, f.profile, f.provider)
	changed := profile != f.profile || provider != f.provider
	f.profile, f.provider = profile, provider
	return changed
}

// recordConfinement is the overlay's confinement, else a replayed record's: "default" is none, a retired value errs.
func recordConfinement(own, rec string) (string, error) {
	if own != "" || rec == "" || rec == "default" {
		return own, nil
	}
	if _, _, err := route.Parse(rec); err != nil {
		return "", fmt.Errorf("the replayed record ran under route %q, which no longer exists: pass --overlay with a provider", rec)
	}
	return rec, nil
}

// parsePerms validates --allow/--ask/--deny/--permissions into r.perms; it reruns after a --retry record fills them.
func (r *askRun) parsePerms() (int, bool) {
	f := r.f
	p, err := permission.ParsePermissions(f.permMode, f.permAllow, f.permAsk, f.permDeny)
	if err != nil {
		return r.fail(err, 2)
	}
	r.perms = p
	return 0, false
}

// dispatchMode handles --batch, which ends the run before a single task is built.
func (r *askRun) dispatchMode() (int, bool) {
	f := r.f
	if code, done := r.loopMode(); done {
		return code, true
	}
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

// batchInputs is what --batch and --loop need from the flags: the pool options and the base request.
func batchInputs(r *askRun) (o batchOpts, base agent.RunOptions, code int, done bool) {
	f := r.f
	if code, done := r.resolveLead(); done { // validates --provider/--profile and sets r.lead for batchBase
		return o, base, code, true
	}
	o = batchOpts{Jobs: f.jobs, FailFast: f.failFast, Agent: r.ref, Fallback: f.fallback, Native: f.native, Auto: r.auto, Identity: f.as, Swap: r.swap, Pending: map[string]int{}}
	if r.auto != "" {
		o.Reg, _ = providers.Load() // a failed load keeps the batch's shared pick
	}
	ov, err := resolveOverlay(r.ov, f.env, f.keepEnv, false)
	if err != nil {
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	if code, done := r.export(); done {
		return o, base, code, true
	}
	return o, r.batchBase().ApplyOverlay(r.ctx, ov), 0, false
}

// runBatch runs --batch to completion; every path ends the run.
func (r *askRun) runBatch() (int, bool) {
	o, base, code, done := batchInputs(r)
	if done {
		return code, true
	}
	return runAskBatch(r.ctx, r.fs, r.f.batchSrc, o, base), true
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
	return agent.RunOptions{Model: f.model, ModelTyped: f.modelTyped, Provider: f.profile, Account: r.ov.Account, Lead: r.lead, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, ReadOnly: f.readOnly, NoTools: f.noTools,
		Timeout: f.timeout, TimeoutTyped: f.timeoutTyped, KeepEnv: f.keepEnv, Autonomous: f.autonomous, Fleet: f.fleet, Codemode: f.codemode, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SafeMode: f.safeMode, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: f.env, OutputSchema: r.schema, Agents: r.defs, Permissions: r.perms, Values: values}
}

// resumeModel keeps a resumed session's model and row; a typed --model wins, a replay keeps the record's own route.
func resumeModel(f *askFlags) {
	if f.model != "" {
		return
	}
	profile, model := agent.SessionModel(f.session)
	f.model = model
	if f.retry == "" && f.job == "" {
		f.profile = cmp.Or(f.profile, profile)
	}
}

// resolveRecord reads the task and applies a --job or --retry record over the flags.
func (r *askRun) resolveRecord() (int, bool) {
	f := r.f
	r.prompt = strings.Join(r.pos, " ")
	r.state = r.askState()
	if rc, done := r.applyRecord(); done {
		return rc, true
	}
	var err error
	markTyped(r.fs, f) // again: applyRecord loaded the record's flag, and a --model on this line wins
	resumeModel(f)
	if rc, done := r.guardResubmit(); done {
		return rc, true
	}
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
	if f.recover {
		if rc, done := r.recoverRetry(); done {
			return rc, true
		}
	}
	if f.retry != "" && !f.recover && !helptext.Explicit(r.fs, "session") {
		rec, err := agent.LoadRecord(f.retry)
		if err != nil {
			return r.fail(err, 1)
		}
		if (rec.RanAs == "" || rec.RanAs == rec.Agent) && f.name == rec.Agent { // never another agent's session
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
	return &askState{Name: &f.name, Provider: &f.profile, Model: &f.model, modelTyped: &f.modelTyped, timeoutTyped: &f.timeoutTyped, Effort: &f.effort,
		Workdir: &f.workdir, Session: &f.session, Tools: &f.tools, ReadOnly: &f.readOnly, NoTools: &f.noTools,
		Timeout: &f.timeout, Attach: (*[]string)(&f.attach), KeepEnv: &f.keepEnv, Autonomous: &f.autonomous,
		Fleet: &f.fleet, EnableMemory: &f.enableMemory, Direct: &f.direct, NoPersist: &f.noPersist, SafeMode: &f.safeMode,
		DebugFile: &f.debugFile, SystemPrompt: &f.systemPrompt, AppendSystemPrompt: &f.appendSystemPrompt,
		OutputSchema: &f.outputSchema, Persona: &r.persona, Host: &f.host, Fallback: &f.fallback, Codemode: &f.codemode,
		PermMode: &f.permMode, PermAllow: &f.permAllow, PermAsk: &f.permAsk, PermDeny: &f.permDeny,
		Native: &f.native, Pack: &f.pack, Coworker: &r.recCoworker, rubric: &f.rubric, env: &f.env, lead: &r.lead}
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

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches or runs it and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, doc agentconfig.Overlay,
	env, keepEnv []string, as, format, save, name string, background, native bool, run *armRetry, out io.Writer) int {
	rc := askPrepare(ctx, fs, req, jobRec, doc, env, keepEnv, as, format, save, name, background, native, run, out)
	releaseRecover(ctx)
	return rc
}

// askPrepare is askPrepareAndRun's body.
func askPrepare(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, doc agentconfig.Overlay,
	env, keepEnv []string, as, format, save, name string, background, native bool, run *armRetry, out io.Writer) int {
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
	if dry, ok := ctx.Value(dryKey{}).(dryDoc); ok {
		return dryRun(ctx, req, dry, format, out)
	}
	if err := claimRecover(ctx); err != nil {
		return failAsk(format, name, err)
	}
	admitPending(ctx, jobRec)
	go warmMask() // resolve the vault key while Prepare runs
	prepared, err := run.prepare(ctx, cl, req, jobRec)
	if err != nil {
		return failAskCode(format, name, err, prepareExitCode(err))
	}
	if err := takeClaim(jobRec, prepared.Record.ID); err != nil {
		return failAsk(format, name, err)
	}
	finishRecover(ctx, prepared.Record.ID)
	saveErr := startPrepared(cl, prepared, background)
	if background {
		if saveErr != nil { // the --job child loads this record: without it the job could never start
			return failAsk(format, name, fmt.Errorf("save dispatch record: %w", saveErr))
		}
		return askSpawn(ctx, fs, prepared, native, format, name)
	}
	if run != nil {
		return run.run(ctx, cl, prepared, req)
	}
	return runAskAndReport(ctx, cl, prepared, jobRec != nil, format, save, out)
}

// takeClaim clears the inherited dispatch claim and, unless this is a --job, claims it for id.
func takeClaim(jobRec *agent.Record, id string) error {
	claim := os.Getenv(agent.DispatchClaimEnv)
	if err := proc.Unsetenv(agent.DispatchClaimEnv); err != nil {
		return err
	}
	if jobRec != nil {
		return nil
	}
	return agent.ClaimDispatchID(claim, id)
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
	return !typed || r.f.host != "" || (r.f.profile == "" && r.f.provider == "")
}

// resolveLead turns route flags typed on this command line into a routed lead (agent.LeadFor); a pin keeps --profile.
func (r *askRun) resolveLead() (int, bool) {
	r.typed = r.f.profile
	lead, err := narrowLead(r.ag, r.f, !r.routesItself())
	if err != nil {
		return r.fail(err, 2)
	}
	r.lead = cmp.Or(lead, r.lead)
	return 0, false
}

// narrowLead routes f's --provider/--profile into a lead and f.profile when routed; "" and no change otherwise.
func narrowLead(ag string, f *askFlags, routed bool) (string, error) {
	if !routed {
		return "", nil
	}
	fallback := f.fallback
	if f.session != "" { // a resumed session runs one hop
		fallback = router.FallbackNone
	}
	q := agent.RouteReq{Agent: ag, Model: f.model, Profile: f.profile, Provider: f.provider, Fallback: fallback}
	lead, provider, err := routedLead(q, agent.VaultAgentIsClaude(ag) || f.live)
	if err != nil {
		return "", err
	}
	f.profile = provider
	return lead, nil
}

// prepareExitCode is the code a Prepare error names, else 1.
func prepareExitCode(err error) int {
	if _, ok := errors.AsType[*agent.UnacceptedError](err); ok || errors.Is(err, providers.ErrModelSpec) {
		return 2
	}
	if usage, ok := errors.AsType[interface {
		error
		ExitCode() int
	}](err); ok {
		return usage.ExitCode()
	}
	return 1
}

// admitJob is serve.Admit; a test seam.
var admitJob = serve.Admit

// admitPending holds a detached job (jobRec set) until `agents serve` admits its pool; with no daemon it runs at once.
func admitPending(ctx context.Context, jobRec *agent.Record) {
	if jobRec != nil {
		admitJob(ctx, *jobRec, os.Stderr)
	}
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
		return maskedEvents{wire.NewJSONLSink(out, dispatchID)}
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
		werr = writeEnvelope(os.Stdout, wire.FailureEnvelope(agentName, err))
	case agent.FormatStreamJSON:
		werr = stdoutSink(format, "").Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "ask: failure envelope not written to stdout: %v\n", werr)
	}
}
