package ask

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/worktree"
)

// contractOpen is what ask knows, typed, before the owner's run.
type contractOpen struct {
	Scope      contract.Scope
	Owner      contract.Party
	Limits     contract.Limits
	Facts      contract.Facts
	Parent     string // the owner contract a fleet member joins; empty for an owner
	Dispatch   string // the owner's dispatch id; it becomes the contract id
	Dir        string // the run's working tree, a lane when Facts.Lane
	ProxyScope string // the scoped proxy's scope file when Facts.ScopedProxy
}

// runEnd is how the owner's run ended; anything but endFinished fails the contract.
type runEnd string

const (
	endFinished  runEnd = "finished"
	endDied      runEnd = "died"
	endCancelled runEnd = "cancelled"
	endDeadline  runEnd = "deadline"
)

// contractReturn is the typed data ask holds after the owner's last turn; nothing in it is model prose.
type contractReturn struct {
	End      runEnd
	Gate     *worktree.GateRun  // the typed gate action's JSON, decoded; nil when it never ran
	Verify   *wire.VerifyResult // the spec's verify run; nil when the spec has none
	Handback *wire.Handback     // the run's typed hand-back; nil when the owner never called it
	Tests    contract.Tests     // the hand-back's tests value (task 10.4)
	Method   contract.Method    // the hand-back's method value (task 10.4)
	Report   string             // the path `memory report --job` filed
	Spent    contract.Spent
	Children []contract.Contract // the sub-contracts, for Verify
}

// handback is the typed hand-back with every enum checked.
type handback struct {
	Status  contract.Status
	Summary string
	Unfixed []string
	Tests   contract.Tests
	Method  contract.Method
}

// Evidence codes of a fail record Go wrote because it could not observe a pass.
const (
	missNoResult    = "no-result"    // no typed result reached Go
	missNotFinished = "not-finished" // the gate or verify started and never exited
	missAbsent      = "absent"       // the lane, scope file or report is not there
	missUntyped     = "untyped"      // the hand-back's status, tests or method is not a known value
	missUnexecuted  = "unexecuted"   // the tests term needs an executed run that passed
)

// openContract opens, starts and saves the dispatch's contract with the open terms Go observed.
func openContract(o contractOpen) (contract.Contract, []contract.Problem, error) {
	c, ps := contract.Open(o.Scope, o.Owner, o.Limits, o.Facts)
	if len(ps) > 0 {
		return c, ps, nil
	}
	c.Parent = o.Parent
	c, err := withRecords(c, openRecords(c, o, time.Now()))
	if err == nil {
		c, err = c.Start(o.Dispatch)
	}
	if err == nil {
		err = contract.Save(c)
	}
	return c, nil, err
}

// openRecords observes the open terms c holds: the lane and the proxy scope file.
func openRecords(c contract.Contract, o contractOpen, now time.Time) []contract.TermRecord {
	var out []contract.TermRecord
	if slices.Contains(c.Terms, contract.TermLane) {
		out = append(out, observed(contract.TermLane, isLane(o.Dir, c.Scope.Lane), o.Dir, missAbsent, now))
	}
	if slices.Contains(c.Terms, contract.TermProxy) {
		out = append(out, observed(contract.TermProxy, isProxyScope(o.ProxyScope), o.ProxyScope, missAbsent, now))
	}
	return out
}

// settleContract records the return terms on id's saved contract, then verifies (and closes) or fails it (D3).
func settleContract(id string, r contractReturn) (contract.Contract, error) {
	return contract.Update(id, func(c contract.Contract) (contract.Contract, error) { return settle(c, r, time.Now()) })
}

// settle is settleContract without the store: records, then Verify, Close for a contract with no close terms, or Fail.
func settle(c contract.Contract, r contractReturn, now time.Time) (contract.Contract, error) {
	hb := typedHandback(r)
	c, err := withRecords(c, returnRecords(c, r, hb, now))
	if err != nil {
		return c, err
	}
	if why, failed := failReason(c, r.End, r.Spent); failed {
		return failWith(c, why, hb, r.Spent)
	}
	v, err := c.Verify(r.Children)
	if err != nil {
		return failWith(c, c.FailReason(r.Spent), hb, r.Spent)
	}
	res := result(v, hb, r.Spent)
	if hasCloseTerms(v) {
		v.Handback = &res // the main session's commit and merge verbs Close with it
		return v, nil
	}
	return v.Close(res)
}

// abandonContract fails id's contract when it is not yet terminal: the deferred call for a panic or a cancel.
func abandonContract(id string, why contract.Reason) (contract.Contract, error) {
	return contract.Update(id, func(c contract.Contract) (contract.Contract, error) {
		if c.State().Terminal() {
			return c, nil
		}
		return c.Fail(why)
	})
}

// returnRecords is one record per return term: gate, tests, handback and report.
func returnRecords(c contract.Contract, r contractReturn, hb *handback, now time.Time) []contract.TermRecord {
	out := []contract.TermRecord{gateRecord(r.Gate, now), testsRecord(c, r.Verify, hb, now)}
	if hb != nil { // only a done status passes: BLOCKED and NEEDS_CONTEXT hand back unfinished work
		done := hb.Status == contract.StatusDone || hb.Status == contract.StatusDoneWithConcerns
		out = append(out, observed(contract.TermHandback, done, string(hb.Status), string(hb.Status), now))
	} else {
		out = append(out, failed(contract.TermHandback, handbackMiss(r.Handback), now))
	}
	if slices.Contains(c.Terms, contract.TermLeaves) {
		out = append(out, leavesRecord(c.Children, r.Children, now))
	}
	return append(out, observed(contract.TermReport, isReport(r.Report), r.Report, missAbsent, now))
}

// leavesRecord passes when every sub-contract id closed ok (a re-sent one on its new holder); else it names the rest.
func leavesRecord(ids []string, kids []contract.Contract, now time.Time) contract.TermRecord {
	var bad []string
	for _, id := range ids {
		if !slices.ContainsFunc(kids, func(k contract.Contract) bool { return k.ID == id && k.State() == contract.StateClosed }) {
			bad = append(bad, id)
		}
	}
	if len(bad) > 0 {
		return failed(contract.TermLeaves, strings.Join(bad, ","), now)
	}
	return observed(contract.TermLeaves, true, strconv.Itoa(len(ids))+" closed", "", now)
}

func gateRecord(g *worktree.GateRun, now time.Time) contract.TermRecord {
	switch {
	case g == nil:
		return failed(contract.TermGate, missNoResult, now)
	case g.Exit == nil:
		return failed(contract.TermGate, missNotFinished, now)
	}
	return observed(contract.TermGate, *g.Exit == 0, exitEvidence(*g.Exit), exitEvidence(*g.Exit), now)
}

// testsRecord is the verify run's exit when one ran, else its not-run reason, else the judged verdict where allowed.
func testsRecord(c contract.Contract, v *wire.VerifyResult, hb *handback, now time.Time) contract.TermRecord {
	switch {
	case ran(v):
		return observed(contract.TermTests, v.Exit == 0, exitEvidence(v.Exit), exitEvidence(v.Exit), now)
	case v != nil && v.Reason != "":
		return failed(contract.TermTests, v.Reason, now)
	case !c.Executed && hb.claims(contract.TestsJudged):
		return contract.TermRecord{Term: contract.TermTests, Verdict: contract.VerdictPass, By: contract.ByOwner, At: now}
	}
	return failed(contract.TermTests, missUnexecuted, now)
}

// ran reports whether v is a verify run of a named command that reached an exit.
func ran(v *wire.VerifyResult) bool { return v != nil && v.Reason == "" && v.Command != "" }

// claims reports whether hb is a typed hand-back whose tests value is t.
func (hb *handback) claims(t contract.Tests) bool { return hb != nil && hb.Tests == t }

// typedHandback is r's hand-back; nil unless it was called and its status, tests and method are all known.
func typedHandback(r contractReturn) *handback {
	if r.Handback == nil {
		return nil
	}
	unfixed := make([]string, len(r.Handback.Unfixed)) // prose is masked before it reaches the state file
	for i, s := range r.Handback.Unfixed {
		unfixed[i] = redact.Text(s)
	}
	hb := handback{Status: contract.Status(r.Handback.Status), Summary: redact.Text(r.Handback.Summary),
		Unfixed: unfixed, Tests: r.Tests, Method: r.Method}
	if !slices.Contains(contract.Statuses(), hb.Status) || !slices.Contains(contract.TestsValues(), hb.Tests) ||
		!slices.Contains(contract.Methods(), hb.Method) {
		return nil
	}
	return &hb
}

func handbackMiss(h *wire.Handback) string {
	if h == nil {
		return missNoResult
	}
	return missUntyped
}

// failReason is why c fails now: the run did not finish, or an open or return term it holds has no pass.
func failReason(c contract.Contract, end runEnd, spent contract.Spent) (contract.Reason, bool) {
	switch end {
	case endFinished:
	case endCancelled:
		return contract.ReasonCancelled, true
	case endDeadline:
		return contract.ReasonDeadline, true
	default:
		return contract.ReasonOwnerDead, true
	}
	for _, t := range c.Unmet() {
		if s := contract.StageOf(t); s == contract.StageOpen || s == contract.StageReturn {
			return c.FailReason(spent), true
		}
	}
	return "", false
}

// failWith fails c for why, keeping a typed hand-back and the spend as its result.
func failWith(c contract.Contract, why contract.Reason, hb *handback, spent contract.Spent) (contract.Contract, error) {
	r := contract.Result{Spent: spent}
	if hb != nil {
		r.Status, r.Summary, r.Unfixed, r.Method = hb.Status, hb.Summary, hb.Unfixed, hb.Method
	}
	c.Result = &r
	return c.Fail(why)
}

// result is the close result: the hand-back's values, with tests taken from who recorded the pass.
func result(c contract.Contract, hb *handback, spent contract.Spent) contract.Result {
	tests := contract.TestsExecuted
	for _, rec := range slices.Backward(c.Records) {
		if rec.Term == contract.TermTests {
			if rec.By == contract.ByOwner {
				tests = contract.TestsJudged
			}
			break
		}
	}
	return contract.Result{Status: hb.Status, Summary: hb.Summary, Unfixed: hb.Unfixed, Tests: tests, Method: hb.Method,
		Spent: spent}
}

func hasCloseTerms(c contract.Contract) bool {
	return slices.ContainsFunc(c.Terms, func(t contract.Term) bool { return contract.StageOf(t) == contract.StageClose })
}

func withRecords(c contract.Contract, recs []contract.TermRecord) (contract.Contract, error) {
	var errs []error
	for _, r := range recs {
		n, err := c.WithRecord(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c = n
	}
	return c, errors.Join(errs...)
}

// observed is a Go record: a pass with evidence, else a fail carrying the miss code.
func observed(t contract.Term, ok bool, evidence, miss string, now time.Time) contract.TermRecord {
	if !ok {
		return failed(t, miss, now)
	}
	return contract.TermRecord{Term: t, Verdict: contract.VerdictPass, By: contract.ByGo, Evidence: evidence, At: now}
}

func failed(t contract.Term, miss string, now time.Time) contract.TermRecord {
	return contract.TermRecord{Term: t, Verdict: contract.VerdictFail, By: contract.ByGo, Evidence: miss, At: now}
}

func exitEvidence(code int) string { return "exit " + strconv.Itoa(code) }

// isLane reports whether dir sits inside the repo's worktree lane named name.
func isLane(dir, name string) bool {
	if dir == "" || name == "" {
		return false
	}
	repo, err := worktree.ResolveRepo(dir)
	if err != nil {
		return false
	}
	d := paths.Canon(dir)
	for _, root := range []string{repo.Root, repo.Legacy} {
		if _, ok := paths.RelWithin(filepath.Join(root, name), d); root != "" && ok {
			return true
		}
	}
	return false
}

// isProxyScope reports whether p is a scope file in the contained proxy's scopes dir; the caller binds it.
func isProxyScope(p string) bool {
	return fileIn(filepath.Join(paths.StateDir(), "proxy", "scopes"), p)
}

// isReport reports whether p is a filed report in the memory bank; the caller passes the path JobReport returned.
func isReport(p string) bool { return fileIn(paths.MemorySubDir("reports"), p) }

// fileIn reports whether p is an absolute regular file, not a symlink, whose resolved parent is below dir.
func fileIn(dir, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	if _, in := paths.RelWithin(paths.Canon(dir), paths.Canon(filepath.Dir(p))); !in {
		return false
	}
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// contractKey carries the spec's checked contract on the run's ctx to askPrepare, which opens it per dispatch id.
type contractKey struct{}

func withContract(ctx context.Context, o *contractOpen) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, contractKey{}, o)
}

func askContractOf(ctx context.Context) *contractOpen {
	o, _ := ctx.Value(contractKey{}).(*contractOpen)
	return o
}

// terms is the checklist TermsFor chose; nil without a contract.
func (o *contractOpen) terms() []contract.Term {
	if o == nil {
		return nil
	}
	return contract.TermsFor(o.Facts)
}

// newContractOpen is the spec's contract key as a typed open; contract.Open's problems are returned as one error.
func newContractOpen(t askspec.Task, bf briefFlags) (*contractOpen, error) {
	dir, err := filepath.Abs(cmp.Or(bf.workdir, "."))
	if err != nil {
		return nil, err
	}
	kind, lane, plan := contract.Kind(t.Contract.Kind), laneOf(dir), planOf(dir, bf.planTask)
	if bf.planTask != "" && plan == "" {
		fmt.Fprintf(os.Stderr, "ask: contract: plan task %q is in no single open plan of this repo: no tick term\n", bf.planTask)
	}
	lim, role := t.Contract.Limits, contract.Role(cmp.Or(t.Contract.Role, string(contract.RoleOwner)))
	o := &contractOpen{
		Scope: contract.Scope{Unit: t.Contract.Unit, Kind: kind, Own: t.Own, PlanTask: bf.planTask, Plan: plan,
			Lane: lane, Verify: t.Verify, Report: t.Report},
		Owner:  contract.Party{Role: role, Agent: bf.name, Tier: bf.tier},
		Limits: contract.LimitsFor(kind, role, contract.Limits{Tokens: lim.Tokens, Leaves: lim.Leaves}),
		// Fleet only when the spec asks for leaves: a solo owner owes no leaves term.
		Facts: contract.Facts{Lane: lane != "", PlanTask: plan != "", Fleet: lim.Leaves > 0,
			TestStep: strings.Contains(t.Verify, "go test"), TestRepair: kind == contract.KindTestRepair},
		Parent: t.Contract.Parent, Dir: dir}
	if role == contract.RoleFleet { // a fleet member works in its owner's lane and plan task: only the owner owes them
		o.Facts = contract.Facts{TestStep: o.Facts.TestStep, TestRepair: o.Facts.TestRepair}
	}
	if _, ps := contract.Open(o.Scope, o.Owner, o.Limits, o.Facts); len(ps) > 0 {
		return nil, problemsErr(ps)
	}
	return o, nil
}

// problemsErr is ps as one spec error; nil for none.
func problemsErr(ps []contract.Problem) error {
	if len(ps) == 0 {
		return nil
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return fmt.Errorf("spec contract: %s", strings.Join(s, "; "))
}

// laneOf is the lane dir sits in: its first path element under the repo's worktree root, else "".
func laneOf(dir string) string {
	repo, err := worktree.ResolveRepo(dir)
	if err != nil {
		return ""
	}
	d := paths.Canon(dir)
	for _, root := range []string{repo.Root, repo.Legacy} {
		if root == "" {
			continue
		}
		if rel, ok := paths.RelWithin(root, d); ok && rel != "." {
			return strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		}
	}
	return ""
}

// planOf is the one dated plan of dir's repo holding id as an open task, else "".
func planOf(dir, id string) string {
	if id == "" {
		return ""
	}
	top, _ := paths.CheckoutOf(dir)
	if top == "" {
		return ""
	}
	found := plans.TaskPlans(plans.Home(top), plans.MirrorDests(), id, func(t plans.Task) bool { return !t.Dropped && !t.Done })
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// contractBlock is the owner's brief block: scope, the terms Go records, and the limits, all read from the contract.
func contractBlock(o *contractOpen) string {
	lines := []string{fmt.Sprintf("Unit: %s (%s)", o.Scope.Unit, o.Scope.Kind)}
	if o.Scope.Plan != "" {
		lines = append(lines, fmt.Sprintf("Plan task: %s (%s)", o.Scope.PlanTask, o.Scope.Plan))
	}
	if o.Scope.Lane != "" {
		lines = append(lines, "Lane: "+o.Scope.Lane)
	}
	lines = append(lines, "Terms Go records: "+joinTerms(o.terms()),
		fmt.Sprintf("Limits: tokens %d, leaves %d, depth %d", o.Limits.Tokens, o.Limits.Leaves, o.Limits.Depth),
		"Go checks every term itself; hand back with the handback tool (status, tests, method).")
	return "\n\n## Contract\n\n" + strings.Join(lines, "\n")
}

// startAsk stamps the plan task and opens the spec's contract on a new dispatch record before it is saved.
func startAsk(ctx context.Context, jobRec, rec *agent.Record) error {
	stampPlanTask(ctx, jobRec, rec)
	o := askContractOf(ctx)
	if o == nil || jobRec != nil { // a --job child's contract was opened by its parent
		return nil
	}
	open := *o
	open.Dispatch = rec.ID
	_, ps, err := openContract(open)
	if len(ps) > 0 || err != nil {
		return fmt.Errorf("open contract: %w", errors.Join(problemsErr(ps), err))
	}
	return joinFleet(open.Parent, rec.ID)
}

// joinFleet reserves a leaf of the owner contract parent for child under its lock, before the child spawns.
func joinFleet(parent, child string) error {
	if parent == "" {
		return nil
	}
	add := func(c contract.Contract) (contract.Contract, error) { return c.AddChild(child) }
	if _, err := contract.Update(parent, add); err != nil {
		if _, aerr := abandonContract(child, contract.ReasonLimit); aerr != nil {
			fmt.Fprintf(os.Stderr, "ask: contract %s: abandon after refusal: %v\n", child, aerr)
		}
		return fmt.Errorf("dispatch refused: %w", err)
	}
	return nil
}

// unsavedHeld fails a foreground dispatch whose record never saved while it holds a contract; the reaper would take it.
func unsavedHeld(ctx context.Context, rec agent.Record, format string, saveErr error) (int, bool) {
	if saveErr == nil || askContractOf(ctx) == nil {
		return 0, false
	}
	got, err := abandonContract(rec.ID, contract.ReasonOwnerDead)
	reportSettled(rec.ID, got, err)
	return failAsk(format, rec.Agent, fmt.Errorf("save dispatch record: %w", saveErr)), true
}

// spawnHeld detaches a background dispatch; a job that never started fails the contract opened for it.
func spawnHeld(ctx context.Context, fs *flag.FlagSet, p *agent.Prepared, native bool, format, name string,
	saveErr error) int {
	var code int
	if saveErr != nil { // the --job child loads this record; without it the job never starts
		code = failAsk(format, name, fmt.Errorf("save dispatch record: %w", saveErr))
	} else {
		code = askSpawn(ctx, fs, p, native, format, name)
	}
	if code != 0 && askContractOf(ctx) != nil {
		if rec, err := agent.LoadRecord(p.Record.ID); err != nil || !rec.Alive() { // a started job settles its own
			got, err := abandonContract(p.Record.ID, contract.ReasonOwnerDead)
			reportSettled(p.Record.ID, got, err)
		}
	}
	return code
}

// runSettled runs a prepared foreground dispatch or --job child; one holding a contract settles it in the run.
func runSettled(ctx context.Context, cl *agent.Client, p *agent.Prepared, req agent.RunOptions, run *armRetry,
	isJob bool, format, save string, out io.Writer) int {
	cid := heldID(p.Record)
	if cid == "" {
		if run != nil {
			return run.run(ctx, cl, p, req)
		}
		return runAskAndReport(ctx, cl, p, isJob, format, save, out)
	}
	defer abandonOnPanic(cid)
	defer watchLeaves(ctx, cid)()
	if run != nil {
		return run.run(ctx, cl, p, req) // the arm retry calls settleHeld before it concludes the record
	}
	return runHeld(ctx, cl, p, isJob, format, save, out)
}

// heldID is the id of the working contract rec's dispatch holds: its own, else the one a re-send took over.
func heldID(rec agent.Record) string {
	if holdsContract(rec.ID) {
		return rec.ID
	}
	chain := agent.ResumeChain(rec.ID) // a --recover run: the dispatches it resumed, nearest first
	for _, from := range chain {
		if _, err := contract.Load(from); err != nil {
			continue
		}
		if takeOver(from, rec.ID, chain) {
			return from
		}
		return ""
	}
	return ""
}

// takeOver makes id the holder of working contract cid when its current holder is one id resumed from.
func takeOver(cid, id string, chain []string) bool {
	c, err := contract.Update(cid, func(c contract.Contract) (contract.Contract, error) {
		last := c.Holders[len(c.Holders)-1]
		if c.State() != contract.StateWorking || last == id || (last != cid && !slices.Contains(chain, last)) {
			return c, nil
		}
		return c.Reassign(id)
	})
	return err == nil && c.State() == contract.StateWorking && c.Holders[len(c.Holders)-1] == id
}

// holdsContract reports whether dispatch id holds a working contract; most asks have no contract file at all.
func holdsContract(id string) bool {
	c, err := contract.Load(id)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ask: contract %s: %v\n", id, err)
		if ferr := contract.FailUnreadable(id); ferr != nil { // else it would sit open forever
			fmt.Fprintf(os.Stderr, "ask: contract %s: fail unreadable: %v\n", id, ferr)
		}
	}
	return err == nil && c.State() == contract.StateWorking
}

// runHeld is runAskAndReport with the record held running until the contract settles, so no reaper takes it.
func runHeld(ctx context.Context, cl *agent.Client, p *agent.Prepared, isJob bool, format, save string, out io.Writer) int {
	opts := jobOpts(isJob, format, out)
	attempt := opts
	attempt.Hold = true
	held := &resultHold{Sink: sinkTo(sinkOut(out, format, save), format, p.Record.ID)}
	res, err := cl.Exec(ctx, p, held, attempt)
	settleHeld(ctx, p, res, err) // settled before Conclude by design: the hold keeps the record running meanwhile
	opts.Notify = cl.Notify
	err = p.Record.Conclude(res, err, opts)
	if format == agent.FormatStreamJSON {
		(&armRetry{}).emitFinal(held, p, res, err)
	}
	return reportAsk(out, format, save, p, res, err)
}

// abandonOnPanic fails id's contract when the run panics, then re-panics; deferred directly so recover works.
func abandonOnPanic(id string) {
	if p := recover(); p != nil {
		got, err := abandonContract(id, contract.ReasonOwnerDead)
		reportSettled(id, got, err)
		panic(p)
	}
}

// settleHeld settles the working contract p's dispatch holds from its held result; a cancelled ctx fails it instead.
func settleHeld(ctx context.Context, p *agent.Prepared, res wire.Result, runErr error) {
	id := heldID(p.Record)
	if id == "" {
		return
	}
	var got contract.Contract
	var err error
	if ctx.Err() == nil {
		ret := returnOf(ctx, p.Record, id, res, runErr)
		if ctx.Err() == nil { // a cancel during the settle gate is a cancel, not an unmet gate
			got, err = settleContract(id, ret)
			reportSettled(id, got, err)
			return
		}
	}
	why := contract.ReasonCancelled
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		why = contract.ReasonDeadline
	}
	got, err = abandonContract(id, why)
	reportSettled(id, got, err)
}

// returnOf is what Go observed of rec's held run: its end, verify and hand-back, the filed report, spend and the gate.
func returnOf(ctx context.Context, rec agent.Record, cid string, res wire.Result, runErr error) contractReturn {
	c, err := contract.Load(cid)
	if err != nil {
		return contractReturn{End: endDied}
	}
	r := contractReturn{End: runEndOf(res, runErr), Verify: res.Verify, Handback: res.Handback, Report: rec.Report}
	if res.Handback != nil {
		r.Tests, r.Method = res.Handback.Tests, res.Handback.Method
	}
	r.Report = reportOf(rec, c)
	waitLeaves(ctx, c, r.End)
	r.Spent = contract.SpentOf(c, append(subtreeUses(rec.ID, spentTokens(&res.Usage)), deadUses(c)...))
	if dir := cmp.Or(rec.Tree, rec.Cwd); r.End == endFinished && dir != "" { // "" would gate this process's cwd
		r.Gate = gateOf(ctx, dir, c.Scope.Own)
	}
	for _, ch := range c.Children { // read after the gate: a leaf may close while it runs
		if cc, err := contract.Load(ch); err == nil {
			r.Children = append(r.Children, cc)
		}
	}
	return r
}

// waitLeaves gives a finished owner's open leaves time to close before the gate, and logs those still working.
func waitLeaves(ctx context.Context, c contract.Contract, end runEnd) {
	if end != endFinished || len(c.Children) == 0 {
		return
	}
	if open := settleLeaves(ctx, c.Children); len(open) > 0 {
		fmt.Fprintf(os.Stderr, "ask: contract %s: leaves still working after the settle wait: %s\n",
			c.ID, strings.Join(open, ", "))
	}
}

// reportOf is the report `memory report --job` filed on rec's saved record, else the spec's own report path if filed.
func reportOf(rec agent.Record, c contract.Contract) string {
	if saved, err := agent.LoadRecord(rec.ID); err == nil {
		rec.Report = cmp.Or(saved.Report, rec.Report)
	}
	if !isReport(rec.Report) && isReport(c.Scope.Report) {
		return c.Scope.Report
	}
	return rec.Report
}

// deadUses is the first holder's subtree of a re-sent contract, so a re-sent leaf is charged for the dead attempt.
func deadUses(c contract.Contract) []contract.Use {
	if len(c.Holders) < 2 {
		return nil
	}
	first := 0
	if dead, err := agent.LoadRecord(c.Holders[0]); err == nil {
		u := dead.Usage
		if u == nil && dead.Activity != nil {
			u = &dead.Activity.Usage
		}
		first = spentTokens(u)
	}
	return subtreeUses(c.Holders[0], first)
}

// The settle gate gets 90 s (a cold lane gate takes ~25 s); wholeDeadline (armretry.go) still caps run plus gate.
const settleGateTimeout = 90 * time.Second

// gateOf runs the scoped gate over own; nil when none is found, and a green run with unknown changed files is no pass.
func gateOf(ctx context.Context, dir string, own []string) *worktree.GateRun {
	ctx, cancel := context.WithTimeout(ctx, settleGateTimeout)
	defer cancel()
	g, err := worktree.RunScopedGate(ctx, dir, gateScope(dir, own), false)
	if g.Gate == "" {
		return nil
	}
	if err != nil && g.Exit != nil && *g.Exit == 0 {
		*g.Exit = 1
	}
	return &g
}

// gateScope is own as the directories the scoped gate takes: an owned file stands for its package directory.
func gateScope(dir string, own []string) []string {
	var out []string
	for _, p := range own {
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(dir, p)
		}
		if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
			p = filepath.Dir(p)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// subtreeUses is every dispatch record started at or after root (none earlier descends from it); root's own is held.
func subtreeUses(root string, rootTokens int) []contract.Use {
	stamp, _, _ := strings.Cut(root, "-") // a dispatch id starts with its start stamp, so names sort by start
	ents, _ := os.ReadDir(paths.DispatchDir())
	out := []contract.Use{{ID: root, Tokens: rootTokens}}
	for _, e := range slices.Backward(ents) {
		if e.Name() < stamp {
			break
		}
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || id == root {
			continue
		}
		if r, err := agent.LoadRecord(id); err == nil {
			u := r.Usage
			if u == nil && r.Activity != nil {
				u = &r.Activity.Usage
			}
			out = append(out, contract.Use{ID: r.ID, Parent: r.Parent, Tokens: spentTokens(u)})
		}
	}
	return out
}

// spentTokens counts every input and output token a budget charges, cache reads included (the budget's own sum).
func spentTokens(u *wire.Usage) int {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.OutputTokens
}

// runEndOf is how the held run ended: a cancel, a finished ok run, else a died owner.
func runEndOf(res wire.Result, runErr error) runEnd {
	switch {
	case agent.CutoffOf(runErr) == agent.CutoffDeadline:
		return endDeadline
	case agent.CutoffOf(runErr) == agent.CutoffCancelled || errors.Is(runErr, context.Canceled):
		return endCancelled
	case res.Status == wire.StatusOK && runErr == nil:
		return endFinished
	}
	return endDied
}

func reportSettled(id string, c contract.Contract, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "ask: contract %s: settle: %v\n", id, err)
		return
	}
	line := fmt.Sprintf("ask: contract %s %s", id, c.State())
	if c.Failed != "" {
		line += fmt.Sprintf(" (%s)", c.Failed)
	}
	if u := c.Unmet(); len(u) > 0 {
		line += " · unmet " + joinTerms(u)
	}
	fmt.Fprintln(os.Stderr, line)
}
