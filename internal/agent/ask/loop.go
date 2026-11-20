package ask

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

//go:embed loop-plan.schema.json
var loopPlanSchema string

//go:embed loop-planner.tmpl
var loopPlannerTmpl string

const (
	loopStateVersion  = 1
	loopDigestChars   = 1500 // answer characters per part the planner sees from earlier rounds
	loopMaxParts      = 12   // parts one round may hold
	loopMaxID         = 40   // part id length: "R<round>.<id>-retry" must fit plan_task's 64
	loopDefaultRounds = 4
	statusNeedsMain   = "needs_main"
)

// Stop reasons; loopCodes maps each to the exit code.
const (
	loopDone          = "planner done"
	loopCap           = "round cap reached"
	loopBudget        = "token budget reached (parts already running finish first)"
	loopInterrupted   = "interrupted"
	loopNoProgress    = "no progress: the planner repeated the previous round"
	loopBadPlan       = "round refused: invalid plan"
	loopPlannerFailed = "planner failed"
	loopSaveFailed    = "state not saved"
)

var loopCodes = map[string]int{loopDone: 0, loopCap: 4, loopBudget: 4, loopInterrupted: 4,
	loopNoProgress: 1, loopBadPlan: 1, loopPlannerFailed: 1, loopSaveFailed: 1}

// loopCfg is one invocation's settings; plan and run are the seams the model calls go through.
type loopCfg struct {
	Rounds, Budget int
	Planner        string
	TypedModel     bool // --model typed on the command line: a failed part is reported, not moved
	NoMove         bool // --fallback none
	JSON           bool
	Log            io.Writer
	Out            io.Writer
	plan           func(ctx context.Context, planner, prompt string) (loopPlan, wire.Usage, error)
	run            func(ctx context.Context, round int, parts []loopPart, retry bool, emit func(wire.Envelope))
}

type loopDriver struct {
	cfg   loopCfg
	st    loopFile
	limit int // round number the planner is told is the last
	start time.Time
}

type loopTodo struct {
	round  int
	parts  []loopPart
	resume bool
}

func (d *loopDriver) logf(format string, a ...any) {
	if d.cfg.Log != nil {
		fmt.Fprintf(d.cfg.Log, "loop: "+format+"\n", a...)
	}
}

// runLoop drives rounds until a stop reason, prints the outcome and returns the exit code.
func runLoop(ctx context.Context, cfg loopCfg, st loopFile) int {
	d := &loopDriver{cfg: cfg, st: st, limit: len(st.Rounds) + cfg.Rounds, start: time.Now()}
	if err := d.save(); err != nil {
		return d.finish(d.saveStop(err))
	}
	d.logf("%s: goal hash %s, up to %d round(s)", st.ID, st.GoalHash, cfg.Rounds)
	why := d.loop(ctx)
	return d.finish(why)
}

func (d *loopDriver) loop(ctx context.Context) string {
	for ran := 0; ; ran++ {
		if why := d.gate(ctx, ran); why != "" {
			return why
		}
		todo, why := d.next(ctx)
		if why != "" {
			return why
		}
		if err := d.execute(ctx, todo); err != nil {
			return d.saveStop(err)
		}
	}
}

func (d *loopDriver) gate(ctx context.Context, ran int) string {
	switch {
	case ctx.Err() != nil:
		return loopInterrupted
	case ran >= d.cfg.Rounds:
		return loopCap
	case d.overBudget():
		return loopBudget
	}
	return ""
}

func (d *loopDriver) overBudget() bool { return d.cfg.Budget > 0 && d.st.Tokens >= d.cfg.Budget }

func (d *loopDriver) saveStop(err error) string {
	d.logf("%v", err)
	return loopSaveFailed
}

// next is the interrupted round's unfinished parts, else a freshly planned round.
func (d *loopDriver) next(ctx context.Context) (loopTodo, string) {
	if n := len(d.st.Rounds); n > 0 {
		if parts := pendingParts(d.st.Rounds[n-1]); len(parts) > 0 {
			return loopTodo{round: d.st.Rounds[n-1].N, parts: parts, resume: true}, ""
		}
	}
	round := len(d.st.Rounds) + 1
	p, err := d.planWith(ctx, round)
	if err != nil {
		if ctx.Err() != nil {
			return loopTodo{}, loopInterrupted
		}
		d.logf("planner: %v", err)
		return loopTodo{}, loopPlannerFailed
	}
	d.st.Summary = cmp.Or(p.Summary, d.st.Summary)
	if p.Done || len(p.Parts) == 0 {
		return loopTodo{}, loopDone
	}
	if err := validateParts(p.Parts); err != nil {
		d.logf("round %d refused: %v", round, err)
		return loopTodo{}, loopBadPlan
	}
	if d.sameAsLast(p.Parts) {
		return loopTodo{}, loopNoProgress
	}
	if d.overBudget() {
		return loopTodo{}, loopBudget
	}
	return loopTodo{round: round, parts: p.Parts}, ""
}

func pendingParts(rd loopRound) []loopPart {
	var out []loopPart
	for _, r := range rd.Parts {
		if r.Status != wire.StatusOK && r.Status != statusNeedsMain {
			out = append(out, r.Part)
		}
	}
	return out
}

// sameAsLast: the planner saw the previous round's results and asked for the same ids and prompts, so it is stuck.
func (d *loopDriver) sameAsLast(parts []loopPart) bool {
	n := len(d.st.Rounds)
	if n == 0 || len(d.st.Rounds[n-1].Parts) != len(parts) {
		return false
	}
	for i, r := range d.st.Rounds[n-1].Parts {
		if r.Part.ID != parts[i].ID || r.Part.Prompt != parts[i].Prompt {
			return false
		}
	}
	return true
}

// planWith asks the planner; a failed non-claude planner is asked once more on claude unless --fallback none.
func (d *loopDriver) planWith(ctx context.Context, round int) (loopPlan, error) {
	prompt, err := plannerPrompt(d.st.Goal, round, d.limit, d.digest())
	if err != nil {
		return loopPlan{}, err
	}
	d.logf("round %d: planning on %s", round, d.cfg.Planner)
	p, u, err := d.cfg.plan(ctx, d.cfg.Planner, prompt)
	d.st.Tokens += usageTokens(u)
	if err != nil && d.cfg.Planner != "claude" && d.cfg.NoMove {
		return p, fmt.Errorf("planner %s failed and --fallback none keeps it there: %w", d.cfg.Planner, err)
	}
	if err != nil && d.cfg.Planner != "claude" && ctx.Err() == nil {
		d.logf("planner %s failed (%v); asking claude once", d.cfg.Planner, err)
		p, u, err = d.cfg.plan(ctx, "claude", prompt)
		d.st.Tokens += usageTokens(u)
	}
	return p, err
}

func usageTokens(u wire.Usage) int { return u.InputTokens + u.OutputTokens + u.CacheCreationTokens }

func plannerPrompt(goal string, round, limit int, digest string) (string, error) {
	t, err := template.New("loop").Option("missingkey=error").Parse(loopPlannerTmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]any{"Goal": goal, "Round": round, "Max": limit, "MaxParts": loopMaxParts,
		"MaxID": loopMaxID, "DigestChars": loopDigestChars, "Digest": digest})
	return b.String(), err
}

func (d *loopDriver) digest() string {
	var b strings.Builder
	for _, rd := range d.st.Rounds {
		for _, r := range rd.Parts {
			fmt.Fprintf(&b, "- R%d.%s [%s%s]: %s\n", rd.N, r.Part.ID, r.Status, cmp.Or(", "+r.Kind, ""), r.Answer)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// validateParts checks ids, count, prompts and that no two parts own overlapping paths.
func validateParts(parts []loopPart) error {
	if len(parts) > loopMaxParts {
		return fmt.Errorf("%d parts, at most %d", len(parts), loopMaxParts)
	}
	ids := map[string]bool{}
	for i, p := range parts {
		switch {
		case !batchIDRE().MatchString(p.ID) || len(p.ID) > loopMaxID:
			return fmt.Errorf("part id %q must match %s and be at most %d characters", p.ID, batchIDRE(), loopMaxID)
		case ids[p.ID]:
			return fmt.Errorf("duplicate part id %q", p.ID)
		case strings.TrimSpace(p.Prompt) == "":
			return fmt.Errorf("part %q has no prompt", p.ID)
		}
		ids[p.ID] = true
		if err := ownsDisjoint(p, parts[:i]); err != nil {
			return err
		}
	}
	return nil
}

func ownsDisjoint(p loopPart, before []loopPart) error {
	for _, q := range before {
		for _, a := range p.Owns {
			for _, b := range q.Owns {
				if plans.OwnsOverlap(a, b) {
					return fmt.Errorf("parts %q and %q both own %s (%s)", q.ID, p.ID, a, b)
				}
			}
		}
	}
	return nil
}

func (d *loopDriver) execute(ctx context.Context, td loopTodo) error {
	d.logf("round %d: %d part(s)", td.round, len(td.parts))
	res := d.runParts(ctx, td.round, td.parts, false)
	if !td.resume {
		d.st.Rounds = append(d.st.Rounds, loopRound{N: td.round})
	}
	rd := &d.st.Rounds[len(d.st.Rounds)-1]
	for _, r := range res {
		if i := slices.IndexFunc(rd.Parts, func(x loopResult) bool { return x.Part.ID == r.Part.ID }); i >= 0 {
			rd.Parts[i] = r
		} else {
			rd.Parts = append(rd.Parts, r)
		}
	}
	if err := d.save(); err != nil {
		return err
	}
	if ctx.Err() == nil && !d.overBudget() {
		d.retryFailed(ctx, td.round, rd)
		return d.save()
	}
	return nil
}

// runParts runs parts as one batch; a part with no envelope is reported as an error result.
func (d *loopDriver) runParts(ctx context.Context, round int, parts []loopPart, retry bool) []loopResult {
	byID := map[string]loopPart{}
	for _, p := range parts {
		byID[p.ID] = p
	}
	got := map[string]loopResult{}
	var mu sync.Mutex
	emit := func(env wire.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		r := resultOf(byID[env.TaskID], env)
		got[env.TaskID] = r
		d.st.Tokens += usageTokens(derefUsage(env.Usage))
		d.logf("R%d.%s %s in %s", round, env.TaskID, r.Status, (time.Duration(env.DurationMs) * time.Millisecond).Round(time.Second))
	}
	d.cfg.run(ctx, round, parts, retry, emit)
	out := make([]loopResult, 0, len(parts))
	for _, p := range parts {
		r, ok := got[p.ID]
		if !ok {
			r = loopResult{Part: p, Status: wire.StatusError, Kind: "missing", Dispatches: []string{}}
		}
		out = append(out, r)
	}
	return out
}

func derefUsage(u *wire.Usage) wire.Usage {
	if u == nil {
		return wire.Usage{}
	}
	return *u
}

func resultOf(p loopPart, env wire.Envelope) loopResult {
	r := loopResult{Part: p, Status: cmp.Or(env.Status, wire.StatusError), Ran: env.Agent, Dispatches: []string{},
		Answer: textutil.TruncateRunes(env.Answer, loopDigestChars), Ms: env.DurationMs}
	if env.DispatchID != "" {
		r.Dispatches = append(r.Dispatches, env.DispatchID)
	}
	if env.Error != nil {
		r.Kind = env.Error.Kind
	}
	return r
}

// keepReason is the standing retry policy of a cut-off ask: a typed model, --fallback none or claude itself stays put.
func (d *loopDriver) keep(r loopResult) string {
	return keepReason(d.cfg.TypedModel || r.Part.Model != "", d.cfg.NoMove, r.Ran)
}

// retryFailed retries each failed part once on claude; one that stays failed becomes needs_main.
func (d *loopDriver) retryFailed(ctx context.Context, round int, rd *loopRound) {
	var again []loopPart
	for i := range rd.Parts {
		r := &rd.Parts[i]
		if r.Status == wire.StatusOK || r.Status == statusNeedsMain {
			continue
		}
		if why := d.keep(*r); why != "" {
			d.logf("R%d.%s %s: not moved (%s)", round, r.Part.ID, r.Status, why)
			r.Status = statusNeedsMain
			continue
		}
		again = append(again, r.Part)
	}
	if len(again) == 0 {
		return
	}
	d.logf("retrying %d part(s) once on claude", len(again))
	res := d.runParts(ctx, round, again, true)
	mergeRetries(rd, res, ctx.Err() != nil)
}

// mergeRetries folds retry results into rd; once cancelled, a canceled or missing one stays for a resume to retry.
func mergeRetries(rd *loopRound, res []loopResult, cancelled bool) {
	for _, x := range res {
		if cancelled && (x.Kind == "canceled" || x.Kind == "missing") {
			continue
		}
		i := slices.IndexFunc(rd.Parts, func(y loopResult) bool { return y.Part.ID == x.Part.ID })
		x.Dispatches = append(slices.Clone(rd.Parts[i].Dispatches), x.Dispatches...)
		x.Retried = true
		if x.Status != wire.StatusOK {
			x.Status = statusNeedsMain
		}
		rd.Parts[i] = x
	}
}

func (d *loopDriver) needsMain() []loopResult {
	var out []loopResult
	for _, rd := range d.st.Rounds {
		for _, r := range rd.Parts {
			if r.Status == statusNeedsMain {
				out = append(out, r)
			}
		}
	}
	return out
}

func (d *loopDriver) finish(why string) int {
	d.st.Reason = why
	if err := d.save(); err != nil {
		d.logf("%v", err)
	}
	code := loopCodes[why]
	nm := d.needsMain()
	if code == 0 && len(nm) > 0 {
		code = 1
	}
	if d.cfg.JSON {
		d.printJSON(why, nm)
	} else {
		d.printText(why, nm, code)
	}
	return code
}

func promptHead(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

func (d *loopDriver) printText(why string, nm []loopResult, code int) {
	w := d.cfg.Out
	fmt.Fprintf(w, "loop %s: %s after %d round(s), %d tokens, %s\n", d.st.ID, why, len(d.st.Rounds), d.st.Tokens,
		time.Since(d.start).Round(time.Second))
	if d.st.Summary != "" {
		fmt.Fprintf(w, "summary: %s\n", d.st.Summary)
	}
	for _, r := range nm {
		fmt.Fprintf(w, "needs main: %s\t%s\t%s\n", r.Part.ID, strings.Join(r.Dispatches, ","), promptHead(r.Part.Prompt))
	}
	if code == loopCodes[loopCap] {
		fmt.Fprintf(w, "resume: claude-code agents ask --loop-resume %s\n", d.st.ID)
	}
}

type loopOut struct {
	LoopID    string       `json:"loop_id"`
	Reason    string       `json:"reason"`
	Rounds    []loopRound  `json:"rounds"`
	NeedsMain []loopResult `json:"needs_main"`
	Tokens    int          `json:"tokens"`
	Summary   string       `json:"summary"`
}

func (d *loopDriver) printJSON(why string, nm []loopResult) {
	if nm == nil {
		nm = []loopResult{}
	}
	b, err := json.Marshal(loopOut{d.st.ID, why, d.st.Rounds, nm, d.st.Tokens, d.st.Summary})
	if err != nil {
		d.logf("result not printed: %v", err)
		return
	}
	fmt.Fprintf(d.cfg.Out, "%s\n", b)
}

// loopSink turns the batch's JSONL envelope stream into emit calls, one per complete line.
type loopSink struct {
	buf  []byte
	emit func(wire.Envelope)
	err  error
}

func (s *loopSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		var env wire.Envelope
		if err := json.Unmarshal(s.buf[:i], &env); err != nil {
			s.err = errors.Join(s.err, err)
		} else {
			s.emit(env)
		}
		s.buf = s.buf[i+1:]
	}
}

// loopAsk is the seam the planner's in-process ask goes through; tests replace the whole plan func instead.
var loopAsk func(ctx context.Context, args []string, out io.Writer) int

// init breaks the initialization cycle runCtx -> loopMode -> loopAsk -> runCtx.
func init() {
	loopAsk = func(ctx context.Context, args []string, out io.Writer) int { return runCtx(ctx, args, "", out) }
}

// loopMode handles --loop and --loop-resume, which end the run before a single task is built.
func (r *askRun) loopMode() (int, bool) {
	f := r.f
	if !f.loop && f.loopResume == "" {
		for _, n := range []string{"loop-rounds", "loop-planner", "loop-budget-tokens"} {
			if helptext.Explicit(r.fs, n) {
				return r.fail(fmt.Errorf("--%s needs --loop", n), 2)
			}
		}
		return 0, false
	}
	if err := r.checkLoopFlags(); err != nil {
		return r.fail(err, 2)
	}
	goal, st, err := r.loopGoal()
	if err != nil {
		return r.fail(err, 2)
	}
	cfg := r.loopConfig()
	if f.dryRun {
		return r.loopDry(cfg, goal), true
	}
	if code, done := r.checkArm(); done {
		return code, true
	}
	o, base, code, done := batchInputs(r)
	if done {
		return code, true
	}
	base.ReadOnly, base.Tools, base.NoTools = true, true, false // editing parts need per-part deny rules the pool lacks
	cfg.plan, cfg.run = r.loopPlanFn(), loopRunner(o, base, helptext.Explicit(r.fs, "timeout"))
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runLoop(ctx, cfg, st), true
}

var loopExclusive = []string{"batch", "retry", "job", "background", "session", "live", "save"}

func (r *askRun) checkLoopFlags() error {
	f := r.f
	if proc.InDispatch(r.ctx) {
		return errors.New("--loop runs at top level only: this command runs inside a dispatch")
	}
	for _, n := range loopExclusive {
		if helptext.Explicit(r.fs, n) {
			return fmt.Errorf("--loop cannot be combined with --%s", n)
		}
	}
	switch {
	case f.format == agent.FormatStreamJSON:
		return errors.New("--loop prints text or --format json")
	case f.loopRounds < 1 || f.loopBudget < 0:
		return errors.New("--loop-rounds needs at least 1 and --loop-budget-tokens cannot be negative")
	case f.loopResume != "" && (f.loop || len(r.pos) > 0):
		return errors.New("--loop-resume takes a loop id only: no --loop and no goal")
	}
	return nil
}

// loopGoal is the loop's state: a saved one for --loop-resume, else a new one for the goal (stdin when absent).
func (r *askRun) loopGoal() (string, loopFile, error) {
	if r.f.loopResume != "" {
		st, err := loadLoop(r.f.loopResume)
		return st.Goal, st, err
	}
	goal, err := resolveAskPrompt(strings.Join(r.pos, " "))
	if err != nil || strings.TrimSpace(goal) == "" {
		return "", loopFile{}, cmp.Or(err, errors.New("--loop needs a goal: give it as the task"))
	}
	return goal, newLoopFile(goal), nil
}

func (r *askRun) loopConfig() loopCfg {
	f := r.f
	cfg := loopCfg{Rounds: f.loopRounds, Budget: f.loopBudget, Planner: f.loopPlanner, TypedModel: f.modelTyped,
		NoMove: f.fallback == router.FallbackNone, JSON: f.format == helptext.JSON, Out: r.out, Log: r.out}
	if cfg.JSON {
		cfg.Log = os.Stderr
	}
	return cfg
}

func (r *askRun) loopDry(cfg loopCfg, goal string) int {
	prompt, err := plannerPrompt(goal, 1, cfg.Rounds, "")
	if err != nil {
		return helptext.Fail("ask: loop template: %v", err)
	}
	fmt.Fprintf(r.out, "loop dry-run: planner %s (read-only), parts on %s, <= %d rounds, <= %d parts, %d at once\n",
		cfg.Planner, cmp.Or(r.ref, "claude"), cfg.Rounds, loopMaxParts, r.f.jobs)
	fmt.Fprintf(r.out, "--- schema ---\n%s--- prompt, round 1 ---\n%s\n", loopPlanSchema, prompt)
	return 0
}

func (r *askRun) loopPlanFn() func(context.Context, string, string) (loopPlan, wire.Usage, error) {
	f := r.f
	return func(ctx context.Context, planner, prompt string) (loopPlan, wire.Usage, error) {
		args := []string{"--agent", planner, "--read-only", "--format", "json", "--output-schema", loopPlanSchema}
		for _, kv := range [][2]string{{"workdir", f.workdir}, {"as", f.as}} {
			if kv[1] != "" {
				args = append(args, "--"+kv[0], kv[1])
			}
		}
		if helptext.Explicit(r.fs, "fallback") {
			args = append(args, "--fallback", f.fallback)
		}
		var buf bytes.Buffer
		rc := loopAsk(ctx, append(args, "--", prompt), &buf)
		return parseLoopPlan(buf.Bytes(), rc)
	}
}

func parseLoopPlan(b []byte, rc int) (loopPlan, wire.Usage, error) {
	var env wire.Envelope
	var p loopPlan
	if err := json.Unmarshal(b, &env); err != nil {
		return p, wire.Usage{}, fmt.Errorf("exit %d, no envelope: %w", rc, err)
	}
	u := derefUsage(env.Usage)
	switch {
	case env.Error != nil:
		return p, u, fmt.Errorf("%s: %s", env.Error.Kind, env.Error.Message)
	case env.Status != wire.StatusOK || len(env.Structured) == 0:
		return p, u, fmt.Errorf("status %q with no structured plan", env.Status)
	}
	return p, u, json.Unmarshal(env.Structured, &p)
}

// loopRunner runs a round's parts on the batch pool; a retry moves them to claude with the base route cleared.
func loopRunner(o batchOpts, base agent.RunOptions, timeoutSet bool) func(context.Context, int, []loopPart, bool, func(wire.Envelope)) {
	return func(ctx context.Context, round int, parts []loopPart, retry bool, emit func(wire.Envelope)) {
		oo, bb := o, base
		if retry {
			oo.Agent, oo.Auto, oo.Pending = "claude", "", nil
			bb.Model, bb.ModelTyped, bb.Lead = "", false, ""
		}
		sink := &loopSink{emit: emit}
		runBatch(ctx, loopLines(round, parts, retry), oo, bb, timeoutSet, newClient, sink)
		if sink.err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: ask: loop: unreadable envelope: %v\n", sink.err)
		}
	}
}

func loopLines(round int, parts []loopPart, retry bool) []parsedLine {
	out := make([]parsedLine, 0, len(parts))
	for i, p := range parts {
		tl := batchLine{ID: p.ID, Agent: p.Agent, Model: p.Model, Prompt: p.Prompt, Timeout: p.Timeout,
			PlanTask: fmt.Sprintf("R%d.%s", round, p.ID)}
		if retry {
			tl.Agent, tl.Model, tl.PlanTask = "claude", "", tl.PlanTask+"-retry"
		}
		out = append(out, parsedLine{N: i + 1, Task: tl})
	}
	return out
}
