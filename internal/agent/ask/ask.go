package ask

import (
	"cmp"
	"context"
	"encoding/json"
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
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"

	// Vendor CLIs register their roster rows from package init; claude links via review.go, pi via flags.go.
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
func Cmd(args []string) int { return runCtx(context.Background(), args, os.Stdout) }

// cmdTo is Cmd with the answer and envelope written to out instead of the process stdout.
func cmdTo(args []string, out io.Writer) int { return runCtx(context.Background(), args, out) }

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
	ov          agentconfig.Overlay // the run's one config document: stored, then --overlay, then typed flags (HopAppend)
	typedOv     agentconfig.Overlay // --overlay as typed on this line (ovTyped); its per-run keys count as typed flags
	ovTyped     bool
	inherited   agentconfig.Overlay // the resolved parent document this invocation started under, before its own route
	projected   []string            // the flags typeOverlay set from typedOv; a --retry re-parse sets them again
	flagRoute   bool                // --profile / --provider name a route: it replaces every lower layer
	recCoworker route.Route         // the confinement a --retry/--job record ran on
	confined    bool                // the confinement filled the lead: resolveLead treats it as typed
	schema      string
	jobRec      *agent.Record
	prompt      string
	state       *askState
	lead        string // profile a routed run leads with (--profile session, --provider, or --profile with --fallback)
	swap        bool   // --fallback was typed and allows another model: a typed model may then be swapped
	auto        string // the workspace databricks:auto picked for this run; "" when the confinement was not an auto pick
	// a --job/--retry record's request, for its rotation and fallback-default marks
	replayed    agent.RunOptions
	carryVerify string // the verify a carrying parent passed, taken once at start
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

// envFallback is the process or vault fallback mode; unset reads as none.
func envFallback(ctx context.Context) string {
	mode, _ := router.EffectiveFallback(ctx, "", providers.CustomEnv())
	return mode
}

// runCtx is Cmd under ctx, whose run values (a parent dispatch id) the new dispatch inherits.
func runCtx(ctx context.Context, args []string, out io.Writer) (exit int) {
	ctx, carried := takeCarryVerify(ctx) // before anything snapshots the env or spawns, so no descendant inherits it
	fs, f, args, pos, code, done := parseAsk(args)
	if done {
		return code
	}
	if !helptext.Explicit(fs, "fallback") {
		f.fallback = envFallback(ctx)
		if !router.ValidFallback(f.fallback) {
			return failAskCode(f.format, f.name, fmt.Errorf("%s=%q unknown (%s)", router.EnvFallback, f.fallback, strings.Join(route.FallbackModes, "|")), 2)
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
	if f.action != "" { // a typed no-model step: no routing, no agent
		return runAction(ctx, fs, f, out)
	}
	if f.dryRun { // a dry-run pick gets one peek mark, the workspace's included
		ctx = agent.PeekContext(ctx)
	} else {
		defer Reap()() // dryness is final once flags and spec/stdin parse; a dry run reaps nothing
	}
	r := &askRun{ctx: withContract(withPlanTask(ctx, f.planTask), f.contract), f: f, fs: fs, args: args, pos: pos, out: out, carryVerify: carried}
	for _, phase := range []func() (int, bool){r.config, r.prelude, r.mergeFlags, r.selectAgent, r.carryMode, r.dispatchMode, r.resolveRecord, r.checkArm, r.resolveLead, r.export} {
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

// defaultTools turns tools on when neither --tools nor --no-tools was typed; a --job/--retry record then sets its own.
func defaultTools(fs *flag.FlagSet, f *askFlags) {
	if !f.noTools && !helptext.Explicit(fs, "tools") {
		f.tools = true
	}
}

func (r *askRun) fail(err error, code int) (int, bool) {
	return failAskCode(r.f.format, r.f.name, err, code), true
}

// config loads the overlay once: a typed one's per-run keys type their flags, a stored one folds under them.
func (r *askRun) config() (int, bool) {
	var err error
	if r.ctx, err = jobAncestry(r.ctx, r.f.job); err != nil { // first: the overlay read must see the parent's fence
		return r.fail(err, 1)
	}
	if r.f.recover && r.f.retry != "" {
		if r.ctx, err = recoveryFence(r.ctx, r.f.retry); err != nil {
			return r.fail(err, 1)
		}
	}
	busy := ""
	if r.stdinTaken() {
		busy = "stdin is the overlay, so give the task as an argument"
	}
	ov, typed, inherited, err := loadOverlays(r.ctx, r.f.overlay, busy)
	if err == nil {
		r.inherited = inherited
		err = takeConfig(r, ov, typed)
	}
	if err != nil {
		return r.fail(err, 2)
	}
	r.f.runner = cmp.Or(r.f.runner, r.ov.Runner)
	r.ov.Runner = r.f.runner // carryOverlay hands it to piRunnerAsked
	markTyped(r.fs, r.f)     // after typeOverlay: an overlay model or timeout is typed like its flag
	defaultTools(r.fs, r.f)
	return 0, false
}

// confinementValue is the fence: overlay's provider[:profile], else a replayed record's; typed route flags drop that.
func confinementValue(r *askRun) (route.Route, error) {
	rec := r.recCoworker
	if r.flagRoute {
		rec = route.Route{}
	}
	return recordConfinement(r.ov.Confinement(), rec)
}

// takeConfig settles the route unit whole and types a typed overlay: it and the flags must agree, flags win the rest.
func takeConfig(r *askRun, ov, typed agentconfig.Overlay) error {
	r.ov = ov
	if err := r.remoteWithHost(); err != nil {
		return err
	}
	f := r.f
	fr := flagRoute(f)
	r.flagRoute = !fr.IsZero()
	fence := f.overlay == "" && proc.Value(r.ctx, route.OverlayVar) != "" // inherited from a parent dispatch
	cmdTyped := f.overlay != ""
	r.typedOv = typed
	if cmdTyped {
		if err := unitConflict(f, fr, typed); err != nil {
			return err
		}
		r.ovTyped = true
		r.projected = typeOverlay(r.fs, typed, nil, true)
	}
	if r.flagRoute && !fence && (!cmdTyped || typed.Provider == "") { // a stored unit yields; a typed one agreed
		r.ov, r.inherited = r.ov.WithoutRoute(), r.inherited.WithoutRoute()
	}
	r.projected = append(r.projected, typeInherited(r.fs, r.ov, nil)...)
	r.ov.Fold(r.fs) // what is left untyped: the stored layer's settings
	return nil
}

// typeInherited types ov's run keys no flag holds (retype: keys a --retry re-sets over the record).
func typeInherited(fs *flag.FlagSet, ov agentconfig.Overlay, retype []string) []string {
	ov.Model, ov.Effort, ov.Fallback, ov.Runner, ov.Host, ov.Agent, ov.Agents = "", "", "", "", "", "", nil
	typedHere := func(n string) bool { return helptext.Explicit(fs, n) && !slices.Contains(retype, n) }
	if typedHere("tools") || typedHere("no-tools") {
		ov.Tools, ov.NoTools = nil, nil // a stored half never refuses a typed flag
	}
	return typeOverlay(fs, settleTools(ov), retype, false)
}

// LoadOverlay is the stored overlay ($COWORKER_OVERLAY, else the vault's) with --overlay value laid over it.
func LoadOverlay(ctx context.Context, value, busy string) (agentconfig.Overlay, error) {
	ov, _, _, err := loadOverlays(ctx, value, busy)
	return ov, err
}

// loadOverlays returns the merged, typed, and inherited overlays; "-" fails with busy when set.
func loadOverlays(ctx context.Context, value, busy string) (merged, typed, inherited agentconfig.Overlay, err error) {
	stored := agentconfig.DispatchOverlayContext(ctx, "")
	switch {
	case busy != "" && cmp.Or(value, stored) == "-":
		from := "--overlay -"
		if value == "" {
			from = route.OverlayVar + "=-"
		}
		return merged, typed, inherited, errors.New(from + ": " + busy)
	case value != "" && stored == "-":
		return merged, typed, inherited, errors.New(route.OverlayVar + "=- with --overlay: stdin is read once, drop one")
	}
	base, err := agentconfig.LoadOverlayFrom(ctx, stored, os.Stdin, false)
	base = localHost(base)
	if err != nil || value == "" {
		return base, typed, base, err
	}
	if typed, err = agentconfig.LoadOverlayFrom(ctx, value, os.Stdin, true); err != nil {
		return merged, agentconfig.Overlay{}, base, err
	}
	if merged, err = base.Derive(settleTools(typed), agentconfig.HopAppend); err != nil {
		return merged, typed, base, fmt.Errorf("overlay: %w", err)
	}
	merged.Schema = cmp.Or(merged.Schema, typed.Schema)
	return localHost(merged), localHost(typed), base, nil
}

// localHost is o with a host naming this machine cleared: it runs here, like --host.
func localHost(o agentconfig.Overlay) agentconfig.Overlay {
	if remote.IsSelf(o.Host) {
		o.Host = ""
	}
	return o
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
		r.ctx, err = carryOverlay(r.ctx, carried(r.ov))
	}
	if err == nil { // the merged layers can outgrow the cap each one met: the --job child must read it back
		_, err = agentconfig.LoadOverlayContext(r.ctx, "", nil)
	}
	if err != nil {
		return r.fail(err, 2)
	}
	return 0, false
}

// carried is ov less the per-run text a child never inherits: the record holds it, and the env doc stays under its cap.
func carried(ov agentconfig.Overlay) agentconfig.Overlay {
	ov.SystemPrompt, ov.AppendSystemPrompt, ov.OutputSchema, ov.DebugFile, ov.Agents = "", "", "", "", nil
	return ov
}

// stdinTaken reports the task, a batch or a framed prompt already reading stdin.
func (r *askRun) stdinTaken() bool {
	f := r.f
	return f.stdinFramed || f.batchSrc == "-" || f.brief == "-" || (taskFromStdin(r.pos) && f.retry == "" && f.job == "" && !f.live)
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
	if err := cmp.Or(checkTypedRoute(f, proc.Value(r.ctx, route.OverlayVar)), dryCombos(f), runnerCheck(f), agentid.ValidEffort(f.name, f.effort), tierCheck(f), planTaskCheck(f), contractHostCheck(f), askCheckFlags(f.env, f.keepEnv, f.format, f.fallback, f.background, f.job, f.tools, f.noTools)); err != nil {
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

// tierCheck refuses a --tier outside the tier list, or on a run that sizes more than one task.
func tierCheck(f *askFlags) error {
	switch {
	case f.tier == "":
		return nil
	case !slices.Contains(agent.TierNames(), f.tier):
		return fmt.Errorf("--tier %q unknown (%s)", f.tier, strings.Join(agent.TierNames(), "|"))
	case f.batchSrc != "" || f.live || f.loop || f.loopResume != "":
		return errors.New("--tier sizes one task: not with --batch, --live or --loop (put a Tier: line in each brief)")
	}
	return nil
}

var ancestorContext = agent.AncestorContext

// jobAncestry checks ancestry and installs the recovered parent values, except for a --job child, already checked.
func jobAncestry(ctx context.Context, job string) (context.Context, error) {
	if job != "" {
		return ctx, nil
	}
	if err := checkAncestry(ctx); err != nil {
		return ctx, err
	}
	return ancestorContext(ctx), nil
}

// codeModeType turns code mode on for the coworker type, through the flag set so the canonical argv carries it.
func codeModeType(fs *flag.FlagSet, ref string, f *askFlags) {
	if ref == agentid.CoworkerType && f.codemode == "" {
		_ = fs.Set("codemode", "on")
	}
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
	codeModeType(r.fs, r.ref, f)
	if err := agent.ValidateAs(f.as, f.name); err != nil { // against the resolved agent: a claude persona passes
		return r.fail(err, 2)
	}
	if err := resolveCodemode(f, r.codemodeRoute()); err != nil {
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
	if f.host != "" && f.provider != "" { // remote ask carries --profile, never a --provider lead
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
	value = r.poolValue(cmp.Or(value, typedArm(f)))
	agentName, batchLines := confineAgent(r.ov, f, r.ag)
	picks := armAutoPick(r.ctx, value)
	fallback := f.fallback
	if !r.swap && typedModel(f.model) { // an env default never moves a typed model to another agent or arm (R8)
		fallback, f.fallback = route.FallbackNone, route.FallbackNone
	}
	moves := r.typedStart(value, fallback)
	nativeCtx := r.ctx
	ownUnit := r.origins().Of("unit") == agentconfig.OriginFlag
	if ownUnit { // a unit typed as a flag is the run's own route: --native meets only the resolved inherited fence
		nativeCtx = withValues(r.ctx, proc.Values{route.OverlayVar: r.inherited.Doc()})
	}
	// applied here, not in export: CheckArm, ConfineLead and resolveLead read the confinement from the run's values
	ctx, err := ApplyRouteOverlay(r.ctx, value, f.profile, agentName, f.model, fallback)
	if err != nil {
		return refuse(r, err, fallback)
	}
	r.ctx = ctx
	r.markAuto(picks, moves)
	if r.ctx, err = withEndpoint(r.ctx, r.ov.Route()); err != nil {
		return r.fail(err, 2)
	}
	if !ownUnit {
		nativeCtx = r.ctx
	}
	if err := cmp.Or(checkNative(nativeCtx, f.native), CheckArm(r.ctx, r.ag, f.profile, f.provider, f.model, f.direct)); err != nil {
		return r.fail(err, 2)
	}
	if !batchLines {
		r.confined = confineLead(r.ctx, r.ag, f) || r.confined
	}
	return 0, false
}

// markAuto records an auto-picked workspace, including a replay that keeps the recorded route rather than a new fence.
func (r *askRun) markAuto(picks, moves bool) {
	picks = picks || moves // a typed start retries on the next workspace after a rate limit, like auto
	retryAuto := r.f.retry != "" && r.replayed.AutoArm && !r.flagRoute && r.ov.Confinement().IsZero()
	if !picks && !r.jobAutoArm() && !retryAuto {
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
	f.timeout = askTimeout(r.fs, r.ov, f.timeout, f.retry, f.job, f.tools)
	req, rerr := r.request(prompt)
	if rerr != nil {
		return
	}
	req.Fallback = fallback
	agent.RecordRefusal(r.ag, req, nil, err)
}

// poolValue is value, or the pool confinement for --provider databricks alone, a pool pin, or a model the pin lacks.
func (r *askRun) poolValue(value route.Route) route.Route {
	f := r.f
	r.swap = helptext.Explicit(r.fs, "fallback") && f.fallback != route.FallbackNone
	auto := route.Route{Provider: router.RouteDatabricks, Profile: route.AutoArm}
	f.native = ownLoginNative(r.ag, f, value)
	switch {
	case !value.IsZero() || f.profile != "" || f.host != "" || r.jobRec != nil || f.native:
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

// storedWorkspace is the pool pick, except a live or resumed run stays on databricks:<its session's workspace>.
func storedWorkspace(r *askRun, pool route.Route) route.Route {
	reg, err := providers.Load()
	if err != nil || !r.f.live && r.f.session == "" {
		return pool
	}
	q, _, err := agent.RunQuery(r.ctx, reg, agent.ModelQuery{Agent: r.ag}, "")
	if err != nil || q.Profile == "" {
		return pool
	}
	return route.Route{Provider: router.RouteDatabricks, Profile: q.Profile}
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

// withEndpoint lays the unit's endpoint on ctx's confinement of the same provider: the whole unit reaches ctx.
func withEndpoint(ctx context.Context, unit route.Route) (context.Context, error) {
	if unit.Endpoint == "" {
		return ctx, nil
	}
	own, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil || own.Provider != unit.Provider || own.Profile == "" || own.Endpoint == unit.Endpoint {
		return ctx, err
	}
	own.Endpoint = unit.Endpoint
	if err := own.Route().Validate(); err != nil {
		return ctx, err
	}
	return withValues(ctx, proc.Values{route.OverlayVar: own.Doc()}), nil
}

// ApplyCode is the exit code for an ApplyRouteOverlay error: a preflight refusal is 1, the rest usage errors.
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

// recordConfinement is the overlay's confinement, else a replayed record's: a retired value errs.
func recordConfinement(own, rec route.Route) (route.Route, error) {
	if !own.IsZero() || rec.IsZero() || rec.Provider == "default" { // an old record's "default" is none
		return own, nil
	}
	if err := rec.CheckProvider(); err != nil {
		return route.Route{}, fmt.Errorf("the replayed record ran under route %q, which no longer exists: pass --overlay with a provider", route.Join(rec.Provider, rec.Profile))
	}
	return rec, nil
}

// dispatchMode handles --batch, which ends the run before a single task is built.
func (r *askRun) dispatchMode() (int, bool) {
	f := r.f
	if code, done := r.loopMode(); done {
		return code, true
	}
	if f.batchSrc != "" { // --batch picks once from base flags; a single task checks after its record
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
	if err := googleToolsCheck(f); err != nil { // batchBase reads the list without a parse error; check it here
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	o = r.batchOpts()
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
	base, err = r.batchBase()
	if err != nil {
		code, done = r.fail(err, 2)
		return o, base, code, done
	}
	return o, base.ApplyOverlay(r.ctx, ov), 0, false
}

// batchOpts is the pool options the flags give --batch and --loop; TimeoutHeld is read from them, never re-derived.
func (r *askRun) batchOpts() batchOpts {
	f := r.f
	return batchOpts{Jobs: f.jobs, FailFast: f.failFast, Agent: r.ref, Fallback: f.fallback, Native: f.native, Auto: r.auto, Identity: f.as,
		Swap: r.swap, Pending: map[string]int{}, TimeoutHeld: timeoutHeld(r.fs, r.ov)}
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

func (r *askRun) batchBase() (agent.RunOptions, error) {
	f := r.f
	googleTools, err := googleToolNames(f.googleTools)
	if err != nil {
		return agent.RunOptions{}, err
	}
	_, values := agent.RunValues(r.ctx, nil)
	return agent.RunOptions{Model: f.model, ModelTyped: f.modelTyped, Provider: f.profile, Account: r.ov.Account, Lead: r.lead, Effort: f.effort, WorkDir: f.workdir, Tools: f.tools, NoTools: f.noTools,
		Timeout: f.timeout, TimeoutTyped: f.timeoutTyped, KeepEnv: f.keepEnv, Autonomous: f.autonomous, Fleet: f.fleet, Codemode: f.codemode, CodemodeReason: f.codemodeReason, Runner: cmp.Or(f.runner, r.ov.Runner), GoogleTools: googleTools, EnableMemory: f.enableMemory, Direct: f.direct,
		NoPersist: f.noPersist, SystemPrompt: f.systemPrompt, AppendSystemPrompt: f.appendSystemPrompt,
		Env: f.env, OutputSchema: r.schema, Agents: r.defs, Values: values, Rotated: r.replayed.Rotated, FallbackDefaulted: r.fallbackDefaulted(),
		FenceTyped: r.fenceTyped(), TypedFlags: r.f.cli, InheritedOverlay: r.inherited.Doc()}, nil
}

// fenceTyped: a typed --overlay or route flag confined the run; a --job relaunch keeps what its record says.
func (r *askRun) fenceTyped() bool {
	if r.jobRec != nil {
		return r.jobRec.Request.FenceTyped
	}
	o := r.origins()
	return o.Typed("unit") && !r.ov.Confinement().IsZero() || o.Typed("fence")
}

// fallbackDefaulted: --fallback was not typed, and a --job/--retry replay's record says its mode was a default too.
func (r *askRun) fallbackDefaulted() bool {
	untyped := r.fs != nil && !helptext.Explicit(r.fs, "fallback")
	return untyped && (r.f.retry == "" && r.f.job == "" || r.replayed.FallbackDefaulted)
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
	typedTools := f.googleTools
	if rc, done := r.applyRecord(); done {
		return rc, true
	}
	f.googleTools = cmp.Or(typedTools, f.googleTools) // a typed list beats the record's
	var err error
	markTyped(r.fs, f) // again: applyRecord loaded the record's flag; a --model on this line beats it
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
	r.ov.Runner = cmp.Or(f.runner, r.ov.Runner) // a replayed record's runner reaches the chain too
	if err := cmp.Or(finalGoogleTools(f, typedTools), r.replayCodemode()); err != nil {
		return r.fail(err, 2)
	}
	if f.outputSchema != "" { // re-resolve: a --retry/--job record may hold a resolved schema
		if r.schema, err = resolveOutputSchema(f.outputSchema); err != nil {
			return r.fail(err, 2)
		}
	}
	return 0, false
}

// replayCodemode rechecks --codemode after a --retry/--job record replaced the agent, route and mode.
func (r *askRun) replayCodemode() error {
	if r.f.retry == "" && r.f.job == "" {
		return nil
	}
	return resolveCodemode(r.f, r.codemodeRoute())
}

// codemodeRoute is the unit the run will apply: the overlay's, or the confinement a --retry/--job record restores.
func (r *askRun) codemodeRoute() route.Route {
	rt := r.ov.Route()
	if v, err := confinementValue(r); err == nil && !v.IsZero() {
		rt.Provider, rt.Profile = v.Provider, v.Profile
	}
	return rt
}

// resolveCodemode settles f.codemode and its reason for the resolved agent and route, noting a fall back to off.
func resolveCodemode(f *askFlags, rt route.Route) (err error) {
	direct := directRoute(cmp.Or(f.provider, rt.Provider), cmp.Or(f.profile, rt.Profile))
	f.codemode, f.codemodeReason, err = validateCodemode(f.codemode, f.name, f.runner, f.tools, direct)
	noteCodemodeOff(f.name, f.codemodeReason)
	return err
}

// recordTools refuses the tools pair a --job/--retry record completes.
func recordTools(f *askFlags) error {
	if err := askExclusive(f.background, f.job, f.tools, f.noTools); err != nil {
		return fmt.Errorf("%w: the replayed record sets one; type it off (--tools=false or --no-tools=false)", err)
	}
	return nil
}

// resumes reports whether a retry by agent name may reuse rec's session: its own, and one the agent saved.
func resumes(rec agent.Record, name string) bool {
	own := (rec.RanAs == "" || rec.RanAs == rec.Agent) && name == rec.Agent // never another agent's session
	return own && (rec.Agent != "pi" || rec.Request.SessionID != "")        // pi runs --no-session unless one was named
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
		r.ctx = jobRetried(r.ctx, *r.jobRec)
		return r.takeRecordFence(r.jobRec.Request) // a --job record already holds the fold
	}
	if r.ctx, rc, done = loadAskRetry(r.ctx, r.fs, r.args, f.retry, f.format, r.state, &r.prompt); done {
		return rc, true
	}
	if f.recover {
		if rc, done := r.recoverRetry(); done {
			return rc, true
		}
	}
	if f.retry != "" {
		rec, err := agent.LoadRecord(f.retry)
		if err != nil {
			return r.fail(err, 1)
		}
		if err := r.mergeRecordOverlay(rec); err != nil {
			return r.fail(err, 2)
		}
		r.ctx = retryLineage(r.ctx, rec)
		if resumes(rec, f.name) && !f.recover && !helptext.Explicit(r.fs, "session") {
			f.session = cmp.Or(rec.SessionID, f.session)
		}
	}
	r.ov.Fold(r.fs) // the overlay beats a --retry record
	return 0, false
}

// takeRecordFence restores a --job relaunch's typed flags, stored overlay limits, and parent document.
func (r *askRun) takeRecordFence(req agent.RunOptions) (int, bool) {
	if r.jobRec != nil {
		if err := r.mergeRecordOverlay(*r.jobRec); err != nil {
			return r.fail(err, 2)
		}
	}
	r.f.cli, r.inherited = req.TypedFlags, agentconfig.Overlay{}
	if req.InheritedOverlay == "" { // never the env: a relaunch's own context holds the parent's route, not its fence
		return 0, false
	}
	inherited, err := agentconfig.LoadOverlayFrom(r.ctx, req.InheritedOverlay, nil, false)
	if err != nil {
		return r.fail(fmt.Errorf("--job record: inherited overlay: %w", err), 2)
	}
	r.inherited = inherited
	return 0, false
}

// origins computes where each key of the run's overlay came from; nothing stores the result.
func (r *askRun) origins() agentconfig.Provenance {
	typed := agentconfig.Overlay{}
	if r.ovTyped {
		typed = r.typedOv
	}
	return agentconfig.Origins(r.flagOverlayKeys(), typed, r.inherited, r.ov)
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
		Workdir: &f.workdir, Session: &f.session, Tools: &f.tools, NoTools: &f.noTools,
		Timeout: &f.timeout, Attach: (*[]string)(&f.attach), KeepEnv: &f.keepEnv, Autonomous: &f.autonomous,
		Fleet: &f.fleet, EnableMemory: &f.enableMemory, Direct: &f.direct, NoPersist: &f.noPersist,
		DebugFile: &f.debugFile, SystemPrompt: &f.systemPrompt, AppendSystemPrompt: &f.appendSystemPrompt,
		OutputSchema: &f.outputSchema, Persona: &r.persona, Host: &f.host, Fallback: &f.fallback, Codemode: &f.codemode, Runner: &f.runner, GoogleTools: &f.googleTools, Verify: &f.verify,
		Native: &f.native, Pack: &f.pack, Coworker: &r.recCoworker, rubric: &f.rubric, env: &f.env, lead: &r.lead, replayed: &r.replayed,
		retype: func() {
			if r.ovTyped {
				typeOverlay(r.fs, r.typedOv, r.projected, true)
			}
			typeInherited(r.fs, r.ov, r.projected) // the parent's fence keys beat the record; typed flags beat both
			if r.ov.Timeout != "" && !r.origins().Typed("timeout") {
				r.f.timeoutTyped = false // an inherited deadline replaced the record's, typed or not
			}
		}}
}

// request is batchBase plus the single-task fields.
func (r *askRun) request(prompt string) (agent.RunOptions, error) {
	f := r.f
	if f.retry == "" && f.job == "" { // a replayed record keeps its own verify
		f.verify = cmp.Or(f.verify, r.carryVerify)
	}
	o, err := r.batchBase()
	if err != nil {
		return o, err
	}
	o.Prompt, o.SessionID, o.Attach, o.DebugFile, o.Pack, o.Agent = prompt, f.session, []string(f.attach), f.debugFile, f.pack, r.ref
	o.Persona, o.Fallback, o.Native, o.Host, o.Rubric, o.Background = r.persona, f.fallback, f.native, f.host, f.rubric, f.background
	o.AutoArm, o.Verify = r.auto != "", f.verify
	return o, nil
}

// askPrepareAndRun layers the overlay over req, prepares the dispatch, then detaches or runs it and reports.
func askPrepareAndRun(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, doc agentconfig.Overlay,
	env, keepEnv []string, as, format, save, name string, background, native bool, run *armRetry, out io.Writer) int {
	rc := askPrepare(ctx, fs, req, jobRec, doc, env, keepEnv, as, format, save, name, background, native, run, out)
	releaseRecover(ctx)
	return rc
}

// overlayedReq applies the resolved overlay to req and checks the agent can run it.
func overlayedReq(ctx context.Context, req agent.RunOptions, doc agentconfig.Overlay, env, keepEnv []string, name string,
	background bool) (agent.RunOptions, error) {
	ov, err := resolveOverlay(doc, env, keepEnv, background)
	if err != nil {
		return req, err
	}
	req = req.ApplyOverlay(ctx, ov)
	return req, agent.CheckCapabilities(name, req)
}

// askPrepare is askPrepareAndRun's body.
func askPrepare(ctx context.Context, fs *flag.FlagSet, req agent.RunOptions, jobRec *agent.Record, doc agentconfig.Overlay,
	env, keepEnv []string, as, format, save, name string, background, native bool, run *armRetry, out io.Writer) int {
	req, err := overlayedReq(ctx, req, doc, env, keepEnv, name, background)
	if err != nil {
		return failAskCode(format, name, err, 2)
	}
	cl := newClient()
	ctx = providers.WithIdentity(ctx, cmp.Or(as, providers.IdentityHeadless)) // headless: a paired row tries its SP first
	req.Identity = cmp.Or(as, req.Identity)                                   // recorded: replay/reboot keeps a typed --as
	if dry, ok := ctx.Value(dryKey{}).(dryDoc); ok {
		return dryRun(ctx, req, dry, format, out)
	}
	if err := claimRecover(ctx); err != nil {
		return failAsk(format, name, err)
	}
	prepared, err := prepareAdmitted(ctx, cl, req, jobRec, run)
	if err != nil {
		return failAskCode(format, name, err, prepareExitCode(err))
	}
	if err := takeClaim(jobRec, prepared.Record.ID); err != nil {
		if jobRec != nil {
			agent.RecordRefusal(jobRec.Agent, req, jobRec, err)
		}
		return failAsk(format, name, err)
	}
	finishRecover(ctx, prepared.Record.ID)
	if err := startAsk(ctx, jobRec, &prepared.Record); err != nil {
		return failAsk(format, name, err)
	}
	markRetryOf(ctx, &prepared.Record, background)
	saveErr := startPrepared(cl, prepared, background)
	if background {
		return spawnHeld(ctx, fs, prepared, native, format, name, saveErr)
	}
	if code, unsaved := unsavedHeld(ctx, prepared.Record, format, saveErr); unsaved {
		return code
	}
	return runSettled(ctx, cl, prepared, req, run, jobRec != nil, format, save, out)
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

// markRetryOf names the retried run on a background retry's record, for its --job child.
func markRetryOf(ctx context.Context, rec *agent.Record, background bool) {
	if r, ok := agent.RetriedOf(ctx); ok && background {
		rec.RetryOf = r.ID
	}
}

// jobRetried makes a background retry's child replay the plan its parent took from the retried run.
func jobRetried(ctx context.Context, rec agent.Record) context.Context {
	if rec.RetryOf == "" {
		return ctx
	}
	return agent.WithRetried(ctx, agent.Retried{ID: rec.RetryOf, Plan: rec.RoutePlan})
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
	lead, err := narrowLead(r.ag, r.f, !r.routesItself(), fencedWorkspace(r.ctx, r.f))
	if err != nil {
		return r.fail(err, 2)
	}
	r.lead = cmp.Or(lead, r.lead)
	return 0, false
}

// fencedWorkspace is the workspace the run's confinement names for a provider-only --provider databricks ask, else "".
func fencedWorkspace(ctx context.Context, f *askFlags) string {
	if f.provider != router.RouteDatabricks || f.profile != "" {
		return ""
	}
	if p := router.ArmProfileContext(ctx); p != route.AutoArm {
		return p
	}
	return ""
}

// narrowLead routes f's --provider/--profile (a fenced workspace first) into a lead and f.profile when routed.
func narrowLead(ag string, f *askFlags, routed bool, fenced string) (string, error) {
	if !routed {
		return "", nil
	}
	fallback := f.fallback
	if f.session != "" { // a resumed session runs one hop
		fallback = route.FallbackNone
	}
	q := agent.RouteReq{Agent: ag, Model: f.model, Profile: cmp.Or(f.profile, fenced), Provider: f.provider, Fallback: fallback}
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
		agent.NoteQueued(jobRec.ID, serve.PoolOf(*jobRec), os.Stderr)
		defer agent.NoteQueued(jobRec.ID, "", os.Stderr)
		admitJob(ctx, *jobRec, os.Stderr)
	}
}

func prepareAdmitted(ctx context.Context, cl *agent.Client, req agent.RunOptions, jobRec *agent.Record, run *armRetry) (*agent.Prepared, error) {
	admitPending(ctx, jobRec)
	err := ctx.Err()
	var prepared *agent.Prepared
	if err == nil {
		go warmMask() // resolve the vault key while Prepare runs
		prepared, err = run.prepare(ctx, cl, req, jobRec)
	}
	if err != nil && jobRec != nil {
		agent.RecordRefusal(jobRec.Agent, req, jobRec, err)
	}
	return prepared, err
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
		return json.NewEncoder(os.Stdout).Encode(struct {
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
