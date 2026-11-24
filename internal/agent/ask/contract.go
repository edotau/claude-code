package ask

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

// contractOpen is what ask knows, typed, before the owner's run.
type contractOpen struct {
	Scope      contract.Scope
	Owner      contract.Party
	Limits     contract.Limits
	Facts      contract.Facts
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
		out = append(out, observed(contract.TermProxy, fileExists(o.ProxyScope), o.ProxyScope, missAbsent, now))
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
	if why, failed := failReason(c, r.End); failed {
		return failWith(c, why, hb, r.Spent)
	}
	v, err := c.Verify(r.Children)
	if err != nil {
		return failWith(c, contract.ReasonTerm, hb, r.Spent)
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
	if hb != nil {
		out = append(out, observed(contract.TermHandback, true, string(hb.Status), "", now))
	} else {
		out = append(out, failed(contract.TermHandback, handbackMiss(r.Handback), now))
	}
	return append(out, observed(contract.TermReport, fileExists(r.Report), r.Report, missAbsent, now))
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

// testsRecord fails on any red verify run; else an executed run passes it, or the judged verdict where none is needed.
func testsRecord(c contract.Contract, v *wire.VerifyResult, hb *handback, now time.Time) contract.TermRecord {
	needRun := c.Executed || hb.claims(contract.TestsExecuted)
	if v != nil && v.Reason == "" && (v.Exit != 0 || needRun) {
		return observed(contract.TermTests, v.Exit == 0, exitEvidence(v.Exit), exitEvidence(v.Exit), now)
	}
	if !needRun && hb.claims(contract.TestsJudged) {
		return contract.TermRecord{Term: contract.TermTests, Verdict: contract.VerdictPass, By: contract.ByOwner, At: now}
	}
	if v != nil && v.Reason != "" {
		return failed(contract.TermTests, v.Reason, now)
	}
	return failed(contract.TermTests, missUnexecuted, now)
}

// claims reports whether hb is a typed hand-back whose tests value is t.
func (hb *handback) claims(t contract.Tests) bool { return hb != nil && hb.Tests == t }

// typedHandback is r's hand-back; nil unless it was called and its status, tests and method are all known.
func typedHandback(r contractReturn) *handback {
	if r.Handback == nil {
		return nil
	}
	hb := handback{Status: contract.Status(r.Handback.Status), Summary: r.Handback.Summary,
		Unfixed: r.Handback.Unfixed, Tests: r.Tests, Method: r.Method}
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
func failReason(c contract.Contract, end runEnd) (contract.Reason, bool) {
	switch end {
	case endFinished:
	case endCancelled:
		return contract.ReasonCancelled, true
	default:
		return contract.ReasonOwnerDead, true
	}
	for _, t := range c.Unmet() {
		if s := contract.StageOf(t); s == contract.StageOpen || s == contract.StageReturn {
			return contract.ReasonTerm, true
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

// fileExists reports whether p is a regular file.
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
