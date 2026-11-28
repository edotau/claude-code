package ask

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	usagepkg "github.com/edotau/claude-code/internal/dev/usage"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/remote/network/remote"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
)

// RouteFlags holds the ask flags the wave runner shares: --overlay and --fallback.
type RouteFlags struct{ f askFlags }

// RegisterRouteFlags adds those flags to fs through ask's own definitions.
func RegisterRouteFlags(fs *flag.FlagSet) *RouteFlags {
	r := &RouteFlags{}
	registerFallbackFlag(fs, &r.f)
	registerOverlayFlag(fs, &r.f)
	return r
}

// Args re-emits the route flags as a single --overlay document, so a child ask run routes as the caller did.
func (r *RouteFlags) Args() ([]string, error) {
	f := &r.f
	if f.overlay == "-" {
		return nil, errors.New("--overlay - reads stdin, which a batch child needs; pass a file or inline JSON")
	}
	if f.fallback == "" {
		if f.overlay == "" {
			return nil, nil
		}
		return []string{"--overlay", f.overlay}, nil
	}
	var base agentconfig.Overlay
	if f.overlay != "" {
		var err error
		if base, err = agentconfig.LoadOverlayFrom(context.Background(), f.overlay, nil, true); err != nil {
			return nil, err
		}
	}
	ov, err := base.Derive(agentconfig.Overlay{Fallback: f.fallback}, agentconfig.HopAppend)
	if err != nil {
		return nil, err
	}
	return []string{"--overlay", ov.Doc()}, nil
}

// failSpawn finishes a record whose spawn failed so it never shows as running; a failed save joins the returned error.
func failSpawn(rec *agent.Record, err error) error {
	cur, _, uerr := agent.UpdateRunning(rec.ID, func(r agent.Record) agent.Record { // only a still-running record
		r.Finish(wire.Result{}, err, time.Since(r.Time))
		return r
	})
	if errors.Is(uerr, os.ErrNotExist) { // never saved: write it settled
		rec.Finish(wire.Result{}, err, time.Since(rec.Time))
		uerr = rec.Save()
	} else if uerr == nil {
		*rec = cur
	}
	if uerr != nil {
		err = errors.Join(err, fmt.Errorf("save failed dispatch %s: %w", rec.ID, uerr))
	}
	return err
}

// RouteRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var RouteRefresh = usagepkg.RefreshAccount

// newClient builds the dispatch client; RouteRefresh is the offline-able account refresh.
func newClient() *agent.Client {
	return agent.NewClient(router.RuleClassifier{}.Alias, RouteRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}

// applyRecordTo copies a logged dispatch's request onto r's flags via ReplayOverlay; env notes go to errw.
func (r *askRun) applyRecordTo(rec agent.Record, errw io.Writer) error {
	ov, _, err := rec.ReplayOverlay(nil, agentconfig.Overlay{})
	if err != nil {
		return err
	}
	f, u := r.f, ov.Route()
	f.name, f.profile, f.model, f.effort = rec.Agent, replayedProfile(rec.Request.Provider, u), cmp.Or(ov.Model, rec.Request.Model), rec.Request.Effort
	f.workdir, f.session, f.tools, f.timeout = rec.Request.WorkDir, rec.Request.SessionID, rec.Request.Tools, rec.Request.Timeout
	f.attach, f.autonomous, f.fleet, f.enableMemory = attachFlag(rec.Request.Attach), rec.Request.Autonomous, rec.Request.Fleet, rec.Request.EnableMemory
	f.keepEnv, f.direct, f.noTools = rec.Request.KeepEnv, rec.Request.Direct, rec.Request.NoTools
	f.noPersist, f.debugFile = rec.Request.NoPersist, rec.Request.DebugFile
	f.systemPrompt, f.appendSystemPrompt, f.outputSchema = rec.Request.SystemPrompt, rec.Request.AppendSystemPrompt, rec.Request.OutputSchema
	f.host, f.codemode, f.verify = rec.Host, rec.Request.Codemode, rec.Request.Verify
	if rec.Request.Runner != "" { // records from before the field keep this run's runner
		f.runner = rec.Request.Runner
	}
	if len(rec.Request.GoogleTools) > 0 {
		f.googleTools = strings.Join(rec.Request.GoogleTools, ",")
	}
	f.native, f.pack, r.persona = rec.Request.Native, rec.Request.Pack, rec.Request.Persona
	if fb := cmp.Or(ov.Fallback, rec.FallbackPolicy); fb != "" { // records from before the field keep this run's default
		f.fallback = fb
	}
	if !u.IsZero() { // the workspace the run was confined to; an overlay provider still wins (checkArm)
		r.recCoworker = u
	}
	if len(rec.Request.EnvNames) > 0 {
		fmt.Fprintf(errw, "ask: the logged run set env %s; values are not stored, so pass --env or --overlay again\n", strings.Join(rec.Request.EnvNames, ","))
	}
	return nil
}

// replayedProfile is the profile a replay leads on: a recorded one follows the unit that answered, unset stays unset.
func replayedProfile(recorded string, u route.Route) string {
	if recorded == "" || u.Profile == "" || u.Profile == route.AutoArm {
		return recorded
	}
	return u.Profile
}

// piRunnerTyped: this line typed runner pi, which cannot carry a recorded --as identity.
func (r *askRun) piRunnerTyped() bool {
	return r.f.runner == route.RunnerPi && (helptext.Explicit(r.fs, "runner") || r.typedOv.Runner != "")
}

// mergeRecordOverlay replays rec's stored overlay limits and unit under any keys typed on this command line.
func (r *askRun) mergeRecordOverlay(rec agent.Record) error {
	typed := agentconfig.Overlay{}
	if r.ovTyped {
		typed = r.typedOv
	}
	ov, _, err := rec.ReplayOverlay(r.flagOverlayKeys(), typed)
	if err != nil {
		return err
	}
	limits := agentconfig.Overlay{MaxChildren: ov.MaxChildren, MaxTreeTokens: ov.MaxTreeTokens,
		MaxDepth: ov.MaxDepth, BudgetUSD: ov.BudgetUSD, BudgetCredits: ov.BudgetCredits}
	if r.ov, err = r.ov.Derive(limits, agentconfig.HopFallback); err != nil {
		return err
	}
	if !r.flagRoute && (!r.ovTyped || r.typedOv.Route().IsZero()) {
		r.ov.Endpoint = cmp.Or(r.ov.Endpoint, ov.Endpoint)
		r.ov.Account = cmp.Or(r.ov.Account, ov.Account)
	}
	if !helptext.Explicit(r.fs, "as") { // --as is what reaches the credentials: a typed overlay's beats the record's
		r.f.as = cmp.Or(typed.Identity, rec.Request.Identity)
		if !r.flagRoute && !r.piRunnerTyped() { // a typed route is a new unit; a typed pi runner takes no recorded identity
			r.f.as = cmp.Or(r.f.as, ov.Identity)
		}
	}
	return nil
}

func (r *askRun) flagOverlayKeys() []string {
	var keys []string
	for _, name := range r.f.cli {
		if k, ok := askKeys[name]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// retypeOverlay sets the typed --overlay's and parent fence's keys again after a --retry re-parse.
func (r *askRun) retypeOverlay() {
	if r.ovTyped {
		typeOverlay(r.fs, r.typedOv, r.projected, true)
	}
	typeInherited(r.fs, r.ov, r.projected) // the parent's fence keys beat the record; typed flags beat both
	if r.ov.Timeout != "" && !r.origins().Typed("timeout") {
		r.f.timeoutTyped = false // an inherited deadline replaced the record's, typed or not
	}
}

// resolveRecord reads the task and applies a --job or --retry record over the flags.
func (r *askRun) resolveRecord() (int, bool) {
	f := r.f
	r.prompt = strings.Join(r.pos, " ")
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
	if r.jobRec, rc, done = loadAskJob(r); done {
		return rc, true
	}
	if r.jobRec != nil {
		r.ctx = jobRetried(r.ctx, *r.jobRec)
		return r.takeRecordFence(r.jobRec.Request) // a --job record already holds the fold
	}
	if rc, done = loadAskRetry(r); done {
		return rc, true
	}
	if f.recover {
		if rc, done := r.recoverRetry(); done {
			return rc, true
		}
	}
	if rc, done := r.applyRetryRecord(); done {
		return rc, true
	}
	r.ov.Fold(r.fs) // the overlay beats a --retry record
	return 0, false
}

func (r *askRun) applyRetryRecord() (int, bool) {
	f := r.f
	if f.retry == "" {
		return 0, false
	}
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
	return 0, false
}

// jobRetried makes a background retry's child replay the plan its parent took from the retried run.
func jobRetried(ctx context.Context, rec agent.Record) context.Context {
	if rec.RetryOf == "" {
		return ctx
	}
	return agent.WithRetried(ctx, agent.Retried{ID: rec.RetryOf, Plan: rec.RoutePlan})
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

// loadAskJob loads a --job record for a background run continuation; rc/done are set only on an early return.
func loadAskJob(r *askRun) (rec *agent.Record, rc int, done bool) {
	f := r.f
	if f.job == "" {
		return nil, 0, false
	}
	claimed, err := agent.ClaimJob(f.job) // claims the record for this process
	if errors.Is(err, agent.ErrJobNotFresh) {
		return nil, failAskCode(f.format, f.name, err, 2), true
	}
	if err != nil {
		return nil, failAsk(f.format, f.name, err), true
	}
	if err := r.applyRecordTo(claimed, os.Stderr); err != nil {
		return nil, failAskCode(f.format, f.name, err, 2), true
	}
	f.modelTyped, f.timeoutTyped = claimed.Request.ModelTyped, claimed.Request.TimeoutTyped
	r.lead, r.replayed = claimed.Request.Lead, claimed.Request // the detached child leads with the profile the parent resolved
	r.prompt, f.rubric = claimed.Request.Prompt, ""
	return &claimed, 0, false
}

// loadAskRetry loads a --retry record, re-applies flags typed here, and marks r.ctx with the retried id and plan.
func loadAskRetry(r *askRun) (rc int, done bool) {
	f := r.f
	if f.retry == "" {
		return 0, false
	}
	rec, err := agent.LoadRecord(f.retry)
	if err != nil {
		return failAsk(f.format, f.name, err), true
	}
	// The logged request wins; only flags typed on this command line override it.
	if err := r.applyRecordTo(rec, os.Stderr); err != nil {
		return failAskCode(f.format, f.name, err, 2), true
	}
	f.modelTyped, f.timeoutTyped = rec.Request.ModelTyped, rec.Request.TimeoutTyped
	r.lead, r.replayed = replayedProfile(rec.Request.Lead, r.recCoworker), rec.Request // the routed lead is not a flag: the replay leads with it
	clearTyped(r)
	if _, err := helptext.ParseArgs(r.fs, r.args); err != nil { // re-apply explicit flags over the record
		return failAskCode(f.format, f.name, err, 2), true
	}
	r.retypeOverlay() // before the cut-off move and the plan read them, like the flags just re-parsed
	if err := retryWorkdir(r.fs, &f.workdir, f.host, rec); err != nil {
		return failAskCode(f.format, f.name, err, 2), true
	}
	r.prompt, f.rubric = cmp.Or(r.prompt, rec.Request.Prompt), "" // a typed task replaces the logged one
	fmt.Fprintf(os.Stderr, "ask: retrying %s (%s, was %s)\n", rec.ID, rec.Agent, rec.Status)
	moved := moveOffCutoff(r, rec, os.Stderr, "")
	r.ctx = withPlanTask(r.ctx, cmp.Or(planTaskOf(r.ctx), rec.PlanTask)) // the retry counts against the same task
	r.ctx = agent.WithRetried(r.ctx, agent.Retried{ID: rec.ID, Plan: replayPlan(r.fs, rec, moved)})
	return 0, false
}

// replayPlan is rec's plan for its retry: a cut-off workspace never leads, a claude move is its runner (D5).
func replayPlan(fs *flag.FlagSet, rec agent.Record, moved bool) *agent.RoutePlan {
	if rec.RoutePlan == nil {
		return nil // a record from before route_plan replays as today
	}
	p := *rec.RoutePlan // PlanRoute reads only the replayed fields; slices are replaced, never mutated
	if rec.CutOff() {
		p.Pool, p.Admitted = nil, nil
	}
	if moved {
		p.Agent, p.Barred = agentid.Claude, "a cut-off retry runs claude's own CLI"
		p.Runners = []agent.Runner{{Kind: agent.RunnerCLI, Agent: agentid.Claude}}
	}
	if helptext.Explicit(fs, "runner") {
		p.Runners = nil
	}
	if helptext.Explicit(fs, "fallback") {
		p.Fallback = router.Policy{}
	}
	if helptext.Explicit(fs, "profile") || helptext.Explicit(fs, "provider") {
		p.Pool, p.Admitted, p.Fence, p.Fenced = nil, nil, route.Fence{}, false
	}
	return &p
}

// retryWorkdir pins a retry to the directory the run used (its cwd, tree, else repo) and checks it exists.
func retryWorkdir(fs *flag.FlagSet, workdir *string, host string, rec agent.Record) error {
	if helptext.Explicit(fs, "workdir") || host != "" { // a remote path is not checkable here
		return nil
	}
	dir := cmp.Or(rec.Cwd, rec.Request.WorkDir, rec.Tree, rec.Repo) // Cwd is absolute; a logged --workdir may be relative
	if dir == "" {
		return nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("--retry %s: the logged directory %s no longer exists; pass --workdir <dir> on the retry", rec.ID, dir)
	}
	*workdir = dir
	return nil
}

// keepReason names why a retry stays on its cut-off route, "" when it may move.
func keepReason(typedModel, noFallback bool, ran string) string {
	switch {
	case typedModel:
		return "a typed model; drop --model or add --agent claude to move it"
	case noFallback:
		return "--fallback none; add --agent claude to move it"
	case ran == "claude":
		return "claude failed; add --agent <other> to move it"
	}
	return ""
}

// moveOffCutoff sends a cut-off run's retry to claude when nothing typed pins the route, else says how to move.
func moveOffCutoff(r *askRun, rec agent.Record, errw io.Writer, ago string) (moved bool) {
	if !rec.CutOff() || rec.BudgetKind != "" || (ago == "" && helptext.Explicit(r.fs, "agent")) { // a resubmit always types --agent; a budget end never moves
		return false
	}
	f := r.f
	ran := cmp.Or(rec.RanAs, rec.Agent)
	auto := agent.IsAuto(f.model) // typed auto asks for rotation, not a pin
	typed, none := f.modelTyped || helptext.Explicit(r.fs, "model") && !auto, f.fallback == route.FallbackNone
	id := rec.ID + ago
	if why := keepReason(typed, none, ran); why != "" {
		fmt.Fprintf(errw, "ask: %s cut off (%s) on %s; this retry keeps it (%s)\n", id, rec.Cutoff, ran, why)
		return false
	}
	f.name, f.model, r.lead, r.recCoworker = "claude", "", "", route.Route{}
	force := "--agent " + ran + " forces the original"
	if ago != "" {
		force = "--model or --fallback none keeps the original"
	}
	fmt.Fprintf(errw, "ask: %s cut off (%s) on %s; retrying on claude (%s)\n", id, rec.Cutoff, ran, force)
	return true
}

// guardResubmit reads a plain ask's task early (stdin once) so a resubmit of a recent cutoff can leave its route.
func (r *askRun) guardResubmit() (int, bool) {
	f := r.f
	if f.retry != "" || f.job != "" || f.stdinFramed || f.live || r.persona != nil {
		return 0, false
	}
	prompt, err := resolveAskPrompt(r.prompt)
	if err != nil {
		return failAsk(f.format, f.name, err), true
	}
	r.prompt = prompt // stdin is read once; execute takes it from here
	guardResubmit(r, prompt, os.Stderr, time.Now())
	return 0, false
}

// retryLineage rejoins the retried run's parent only within the caller's own tree, or top-level while it still runs.
func retryLineage(ctx context.Context, rec agent.Record) context.Context {
	if rec.Parent == "" {
		return ctx
	}
	_, values := agent.RunValues(ctx, nil)
	caller := values[proc.DispatchIDEnv]
	if caller == rec.ID { // agents send resumes a run as its own child
		return ctx
	}
	p, err := agent.LoadRecord(rec.Parent)
	if err != nil || (caller == "" && !p.Alive()) || (caller != "" && !inLineage(caller, rec.Parent)) {
		return ctx
	}
	// The parent's own depth, not the caller's, so the rerun lands at parent depth+1.
	depth := strconv.Itoa(p.Depth)
	return proc.WithValues(ctx, values.Merge(proc.Values{proc.DispatchIDEnv: rec.Parent, proc.DispatchDepthEnv: depth}))
}

// inLineage reports whether id is caller or one of its recorded ancestors.
func inLineage(caller, id string) bool {
	for n := 0; caller != "" && n < 64; n++ { // bounded so a corrupt parent cycle cannot spin
		if caller == id {
			return true
		}
		rec, err := agent.LoadRecord(caller)
		if err != nil {
			return false
		}
		caller = rec.Parent
	}
	return false
}

// guardResubmit applies moveOffCutoff to a plain ask whose agent, model and task cut off within the last hour.
func guardResubmit(r *askRun, prompt string, errw io.Writer, now time.Time) {
	e, ok := agent.RecentCutoff(agent.CutoffKey(r.f.name, r.f.model, prompt), now)
	if !ok {
		return
	}
	moveOffCutoff(r, agent.Record{ID: e.ID, Agent: e.Agent, Cutoff: e.Kind}, errw, " ("+now.Sub(e.Time).Truncate(time.Second).String()+" ago)")
}

// clearTyped drops the record's lead and list values for flags typed on this command line; the re-parse sets them.
func clearTyped(r *askRun) {
	r.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "profile", "provider": // an explicit route on this command line replaces the logged lead
			r.lead = ""
		case "attach":
			r.f.attach = nil
		case "keep-env":
			r.f.keepEnv = nil
		case "env":
			r.f.env = nil
		}
	})
}

// recoverKey carries a *recoverClaim: the dispatch a --recover targets and whether this run holds its marker.
type recoverKey struct{}

type recoverClaim struct {
	from  string
	nonce string // the claim's attempt: this run's own, or the watchdog's, handed down in ResumeNonceEnv
	held  bool
	err   error // a failed adoption: the run must not start (startAsk reads it)
}

func recoverOf(ctx context.Context) *recoverClaim {
	c, _ := ctx.Value(recoverKey{}).(*recoverClaim)
	return c
}

// claimRecover takes the one recovery claim on a dispatch, or joins the watchdog's launching one by its nonce.
func claimRecover(ctx context.Context) error {
	c := recoverOf(ctx)
	if c == nil {
		return nil
	}
	rec, err := agent.LoadRecord(c.from)
	if err != nil {
		return err
	}
	if nonce := os.Getenv(agent.ResumeNonceEnv); nonce != "" {
		_ = proc.Unsetenv(agent.ResumeNonceEnv) // this run adopts it once; its children never inherit it
		if !rec.ClaimedBy(nonce) {
			return fmt.Errorf("dispatch %s: the watchdog's claim is gone", c.from)
		}
		c.nonce, c.held = nonce, true
		return nil
	}
	got, nonce, err := agent.ClaimResend(rec.ID) // gate and claim in one record-lock section (10.4)
	if errors.Is(err, agent.ErrHolderLive) {
		return cmp.Or(checkResumable(got), err)
	}
	if err != nil {
		return err
	}
	c.nonce, c.held = nonce, true
	return nil
}

// finishRecover adopts the claim as the new dispatch id before anything starts; a failure is kept in c.err.
func finishRecover(ctx context.Context, id string) {
	if c := recoverOf(ctx); c != nil && c.held {
		c.err = agent.AdoptResend(c.from, c.nonce, id)
	}
}

// releaseRecover drops a --recover claim this run took and never adopted, so a failed start can be retried.
func releaseRecover(ctx context.Context) {
	if c := recoverOf(ctx); c != nil && c.held {
		_ = agent.ReleaseResend(c.from, c.nonce) // an adopted or watchdog-launching claim is not this run's to drop
	}
}

// recoverRetry turns a --retry into a recovery: swap in RecoveryPrompt and start a fresh session; the claim is later.
func (r *askRun) recoverRetry() (int, bool) {
	f := r.f
	if f.retry == "" {
		return r.fail(errors.New("--recover needs --retry <id>"), 2)
	}
	rec, err := agent.LoadRecord(f.retry)
	if err != nil {
		return r.fail(err, 1)
	}
	if rec.Alive() {
		return r.fail(checkResumable(rec), 1)
	}
	r.ctx = context.WithValue(r.ctx, recoverKey{}, &recoverClaim{from: rec.ID})
	if !helptext.Explicit(r.fs, "session") {
		f.session = "" // never inherit the cut-off turn's session
	}
	if r.prompt != rec.Request.Prompt { // a typed task replaces the logged one: nothing to recover into
		return 0, false
	}
	p, err := agent.RecoveryPrompt(rec, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: --recover: %v; plain retry\n", err)
		return 0, false
	}
	r.prompt = p
	fmt.Fprintf(os.Stderr, "ask: recovering %s on a fresh session\n", rec.ID)
	return 0, false
}
