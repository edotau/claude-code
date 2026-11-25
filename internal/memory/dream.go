package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/transcript"
)

// dreamPreview is `memory dream --dry-run`'s deterministic report of the mechanical steps only.
type dreamPreview struct {
	Dir           string
	Oversized     []string // bank files over cap
	Archivable    int      // durable-file overflow blocks that would move to this store's archive/
	SessionRotate int      // sessionHistory entries that would rotate to archive/
	Unreadable    []string // "name: error" for each bank file that could not be read, so was not checked
	HookP95       []dreamBenchEntry
	HookP95Err    error
	RtkMisses     []rtkMiss
	RtkErr        error
}

// Any reports whether the preview found anything a real dream run would change mechanically.
func (p dreamPreview) Any() bool {
	return len(p.Oversized) > 0 || p.Archivable > 0 || p.SessionRotate > 0 || len(p.HookP95) > 0
}

// Report renders the preview as `memory dream --dry-run`'s stdout.
func (p dreamPreview) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bank: %s\n", p.Dir)
	if len(p.Oversized) > 0 {
		fmt.Fprintf(&b, "over cap: %s\n", strings.Join(p.Oversized, ", "))
	}
	if p.Archivable > 0 {
		fmt.Fprintf(&b, "would archive %d block(s)\n", p.Archivable)
	}
	if p.SessionRotate > 0 {
		fmt.Fprintf(&b, "would rotate %d sessionHistory entr(ies) to archive/\n", p.SessionRotate)
	}
	for _, u := range p.Unreadable {
		fmt.Fprintf(&b, "home %s — unreadable, so not checked\n", u)
	}
	writeDreamP95(&b, p.HookP95, p.HookP95Err)
	writeDreamRtk(&b, p.RtkMisses, p.RtkErr)
	if !p.Any() && len(p.Unreadable) == 0 {
		b.WriteString("nothing to consolidate mechanically — a real run still applies the judgment pass (dedup, merge, cross-pollinate)\n")
	}
	return b.String()
}

// previewDream computes dreamPreview without writing anything.
func previewDream() dreamPreview {
	local := statBank() // one read per typed file feeds the cap, archive and rotation checks below
	p := dreamPreview{Dir: paths.MemorySubDir(""), Oversized: local.Oversized, Unreadable: local.Unreadable}
	for name, capLines := range archiveCaps {
		_, _, overflow := overflowSplit(name, string(local.data[name]), capLines) // archiveFile's split, read-only
		p.Archivable += len(overflow)
	}
	_, _, rotate := sessionOverflow(string(local.data["sessionHistory.md"]))
	p.SessionRotate = len(rotate)
	p.HookP95, p.HookP95Err = readDreamP95(filepath.Join(paths.StateDir(), "hooks"), time.Now())
	p.RtkMisses, p.RtkErr = readDreamRtk()
	return p
}

const dreamInterval = 24 * time.Hour

// dreamRetry is how long a first failed dream waits; each consecutive failure doubles it up to dreamInterval.
const dreamRetry = 6 * time.Hour

func retryAfter(fails int) time.Duration {
	d := dreamRetry
	for i := 1; i < fails && d < dreamInterval; i++ {
		d *= 2
	}
	return min(d, dreamInterval)
}

// dreamFailures is the consecutive failed runs recorded in the stamp ("failed" alone counts 1).
func dreamFailures() int {
	data, err := os.ReadFile(dreamStampPath())
	if err != nil {
		return 0
	}
	_, rest, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	f, ok := strings.CutPrefix(rest, "failed")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(f))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// dreamChildEnv marks a process spawned by dream's agent run, where `promote --to claude-md` may print its procedure.
const dreamChildEnv = "CLAUDE_MEMORY_DREAM_CHILD"

// isDreamChild reports whether this process runs inside a dream worker (or one of its children).
func isDreamChild() bool {
	v := os.Getenv(dreamChildEnv)
	return v != "" && v != "0"
}

// dreamStampPath records the last consolidation time.
func dreamStampPath() string {
	return filepath.Join(paths.MemoryStateDir(), "last-dream-"+stateKey())
}

// dreamStamp reads the stamp: its time (line 1) and whether that run failed (line 2 = "failed").
func dreamStamp() (t time.Time, failed, ok bool) {
	data, err := os.ReadFile(dreamStampPath())
	if err != nil {
		return time.Time{}, false, false
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	t, err = time.Parse(time.RFC3339, first)
	return t, strings.HasPrefix(rest, "failed"), err == nil
}

// effectiveStamp is dreamStamp, but a future stamp (clock moved back) reads as none: it would hide every changed file.
func effectiveStamp(now time.Time) (t time.Time, failed, ok bool) {
	if t, failed, ok = dreamStamp(); ok && t.After(now) {
		return time.Time{}, false, false
	}
	return t, failed, ok
}

// dreamDue: the last dream is over a day old (or never ran) and a bank changed since; a failed run is due at its stamp.
func dreamDue(now time.Time) bool {
	stamp, failed, ok := effectiveStamp(now)
	if ok && now.Sub(stamp) < dreamInterval {
		return false
	}
	if ok && failed {
		return true
	}
	names, err := newReports(proposalWatermark().Add(watermarkSlack))
	return newestMtime().After(stamp) || len(names) > 0 || err != nil
}

// dreamOptions drive one consolidation.
type dreamOptions struct {
	CWD   string
	Run   AgentFunc
	Out   io.Writer
	IfDue bool // re-check dreamDue once the lock is held: a racing peer may have just dreamed

	Trigger string // session-start | schedule | manual, for the result record
	Forced  bool
}

// dreamChanged lists the top-level and feedback/ and notes/ store .md files modified after since (all when zero).
func dreamChanged(since time.Time) []string {
	var out []string
	for _, dir := range []string{paths.MemoryStoreDir(), filepath.Join(paths.MemoryStoreDir(), "feedback"), filepath.Join(paths.MemoryStoreDir(), "notes")} {
		for _, f := range transcript.GlobIn(dir, "*.md") {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(since) {
				out = append(out, f)
			}
		}
	}
	return out
}

// agentDreamPrompt frames the consolidation pass; the procedure is one copy, embedded at prompts/dream.md.
func agentDreamPrompt(changed []string) (string, error) {
	ref := assets.MemoryDreamProcedure()
	list := assets.MemoryDreamUnchanged()
	if len(changed) > 0 {
		list = "\n- " + strings.Join(changed, "\n- ")
	}
	return assets.MemoryDreamPrompt(assets.MemoryDreamWorker(paths.MemoryStoreDir()), list, ref), nil
}

// dream runs one consolidation and records it, whatever the outcome, in dream-results.jsonl.
func dream(o dreamOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	begin := time.Now()
	log, _ := openDreamLog() // a log that cannot open loses only the copy; the run goes on
	if log != nil {
		o.Out = io.MultiWriter(o.Out, log)
	}
	res := DreamResult{At: begin, Trigger: o.Trigger, Forced: o.Forced}
	err := dreamRun(o, &res)
	res.DurationMs = time.Since(begin).Milliseconds()
	finishDreamResult(&res, err, log)
	return errors.Join(err, appendDreamResult(res))
}

// finishDreamResult sets the outcome and the log name, and drops the log of a not-due run.
func finishDreamResult(res *DreamResult, err error, log *os.File) {
	switch {
	case err != nil:
		res.Outcome = DreamFailed
		res.Reason, _, _ = strings.Cut(err.Error(), "\n")
	case res.Reason == dreamNotDue:
		res.Outcome = DreamNoop
	case len(res.Changed) == 0 && res.Archived == 0:
		res.Outcome, res.Reason = DreamNoop, "no bank change"
	default:
		res.Outcome = DreamOK
	}
	if log == nil {
		return
	}
	name := log.Name()
	_ = log.Close()
	if res.Reason == dreamNotDue && err == nil {
		_ = os.Remove(name)
		return
	}
	res.Log = name
}

const dreamNotDue = "not due"

// dreamRun archives durable-file overflow, consolidates the bank on the agent, settles, and stamps.
func dreamRun(o dreamOptions, res *DreamResult) error {
	var run dreamRan
	notDue, failures := false, dreamFailures() // read before the in-lock stamp resets the count
	lockErr := paths.WithFileLock(lockPathOf(), func() error {
		if notDue = o.IfDue && !dreamDue(time.Now()); notDue {
			fmt.Fprintln(o.Out, "memory dream: "+dreamNotDue)
			return nil
		}
		run = dreamLocked(o)
		// An interrupted settlement must remain retryable, even when the agent succeeded.
		n := failures + 1
		return stampDream(time.Now().Add(retryAfter(n)-dreamInterval), n)
	})
	if notDue && lockErr == nil {
		res.Reason = dreamNotDue
		return nil
	}
	run.fill(res)
	flags := dreamPerformance(o.Out)
	if err := errors.Join(lockErr, run.runErr, run.archiveErr, dreamSettle(o, flags)); err != nil { // settle after unlocking
		// backdated, so dreamDue retries after the backoff for this many consecutive failures
		n := failures + 1
		return errors.Join(err, stampDream(time.Now().Add(retryAfter(n)-dreamInterval), n))
	}
	return stampDream(time.Now(), 0)
}

// dreamSettle settles the bank and reports a failure on o.Out as well as returning it.
func dreamSettle(o dreamOptions, flags []string) error {
	if _, err := settleBank(time.Now()); err != nil {
		settleErr := fmt.Errorf("settle: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", settleErr)
		return settleErr
	}
	problems := doctor(o.CWD, DefaultStaleDays, time.Now())
	if len(flags) > 0 {
		problems = append([]string{strings.Join(flags, "; ")}, problems...)
	}
	if err := cacheDoctorNotice(problems); err != nil {
		fmt.Fprintln(o.Out, "memory dream: doctor notice:", err)
	}
	return nil
}

// dreamRan is what the locked part of a dream did: its errors, the archive count and the agent's report.
type dreamRan struct {
	runErr, archiveErr error
	archived           int
	changed            []string // bank files the agent modified, store-relative
	job                *JobResult
}

// fill copies the run's facts into the result record.
func (r dreamRan) fill(res *DreamResult) {
	res.Archived, res.Changed = r.archived, r.changed
	if r.job != nil {
		res.Agent, res.Profile, res.Model, res.Local = r.job.Agent, r.job.Profile, r.job.Model, r.job.Local
	}
}

// dreamLocked archives overflow and runs the agent, the bank lock held.
func dreamLocked(o dreamOptions) dreamRan {
	var r dreamRan
	n, err := archiveOverflow(time.Now())
	r.archived = n
	if err != nil {
		r.archiveErr = fmt.Errorf("archive: %w", err)
		fmt.Fprintln(o.Out, "memory dream:", r.archiveErr)
	}
	fmt.Fprintf(o.Out, "memory dream: archived %d block(s)\n", n)
	if o.Run == nil {
		return r
	}
	since, _, _ := effectiveStamp(time.Now()) // no stamp: the zero time lists every file
	changed := dreamChanged(since)
	r.archiveErr = errors.Join(r.archiveErr, dreamIngest(o.Out))
	prompt, err := agentDreamPrompt(changed)
	if err != nil {
		r.runErr = err
		return r
	}
	r.runAgent(o, prompt, len(changed))
	return r
}

// runAgent runs the agent on prompt, prints its receipt and fills r; the plan phase brackets the run.
func (r *dreamRan) runAgent(o dreamOptions, prompt string, nChanged int) {
	plan := startPlanPhase(o.CWD)
	prompt += plan.promptBlock()
	misses, rtkErr := readDreamRtk()
	writeDreamRtk(o.Out, misses, rtkErr)
	prompt += rtkPromptBlock(misses)
	defer plan.finish(o.Out)
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	ctx, r.job = WithJobReport(ctx)
	// UpdateChildEnv keeps a fallback dispatch from re-triggering hooks; dreamChildEnv lets `memory promote` through
	start := time.Now()
	who, err := o.Run(withChildEnv(ctx, UpdateChildEnv+"=1", dreamChildEnv+"=1"), prompt, o.CWD)
	fmt.Fprintln(o.Out, dreamRunLine(who, err, r.job.Local))
	if err != nil && strings.Contains(err.Error(), "\n") {
		fmt.Fprintln(o.Out, "memory dream: error detail:", err) // the run line keeps only the first line
	}
	after := dreamChanged(start)
	for _, f := range after {
		rel, relErr := filepath.Rel(paths.MemoryStoreDir(), f)
		if relErr != nil {
			rel = f
		}
		r.changed = append(r.changed, rel)
	}
	if err == nil && nChanged > 0 && len(after) == 0 {
		fmt.Fprintf(o.Out, "memory dream: the agent edited no bank file (%d changed since the last dream); see its reply\n", nChanged)
	}
	r.runErr = err
	r.archiveErr = errors.Join(r.archiveErr, dreamIngest(o.Out)) // again: a report may land during the run
}

// dreamIngest keeps new reports' proposed tests and reports a failure on out as well as returning it.
func dreamIngest(out io.Writer) error {
	err := ingestTestProposals(out)
	if err != nil {
		err = fmt.Errorf("test proposals: %w", err)
		fmt.Fprintln(out, "memory dream:", err)
	}
	return err
}

// dreamRunLine is the receipt: "ran on" only for a success, else the failure's first line; local runs say so.
func dreamRunLine(who string, err error, local bool) string {
	line := "memory dream: ran on " + who
	if err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		line = "memory dream: failed on " + who + ": " + first
	}
	if local {
		line += " (local model: lower trust)"
	}
	return line
}

// dreamPlans is the plan phase: the session checkout and its plans and citable evidence from before the run.
type dreamPlans struct {
	root   string
	before plans.Snapshot
	cite   plans.Cite
}

// startPlanPhase snapshots the checkout's plans; nil when cwd is no checkout or has no plans home.
func startPlanPhase(cwd string) *dreamPlans {
	top := GitToplevel(cwd)
	if top == "" {
		return nil
	}
	if _, err := os.Stat(plans.Home(top)); err != nil {
		return nil
	}
	return &dreamPlans{root: top, before: plans.TakeSnapshot(top), cite: storeCites()}
}

// promptBlock is the open tasks the plan phase judges, as compact JSON; empty when no recent plan has one.
func (d *dreamPlans) promptBlock() string {
	if d == nil {
		return ""
	}
	open := plans.OpenTasks(d.root)
	b, err := json.Marshal(open)
	if len(open) == 0 || err != nil {
		return ""
	}
	return assets.MemoryOpenTasks(string(b))
}

// finish re-checks every plan change made during the run and prints one line; it never fails the dream.
func (d *dreamPlans) finish(out io.Writer) {
	if d == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	rep, err := plans.RecheckJob(ctx, d.root, JobDream, d.before, d.cite)
	if err == nil && len(rep.Kept)+len(rep.Reverted)+len(rep.Flagged) == 0 && !rep.TextChanged {
		return
	}
	line := rep.Line()
	if err != nil {
		line += "; re-check errors: " + strings.Join(strings.Fields(err.Error()), " ")
	}
	fmt.Fprintln(out, "memory dream:", line)
}

// storeCites is the evidence a strike may cite, read before the run so the job cannot cite what it wrote.
func storeCites() plans.Cite {
	ids := map[string]bool{}
	logs := append([]string{Path("decisionLog.md")}, transcript.GlobIn(filepath.Join(paths.MemoryStoreDir(), "archive"), "decisionLog*.md")...)
	for _, f := range logs {
		b, _ := os.ReadFile(f) // a missing log cites nothing
		for _, m := range idMarker().FindAllStringSubmatch(string(b), -1) {
			ids[m[1]] = true
		}
	}
	notes := map[string]bool{}
	for _, dir := range []string{"feedback", "notes"} {
		for _, f := range transcript.GlobIn(filepath.Join(paths.MemoryStoreDir(), dir), "*.md") {
			notes[dir+"/"+filepath.Base(f)] = true
		}
	}
	return func(kind, ref string) bool { return kind == "decision" && ids[ref] || kind == "note" && notes[ref] }
}

// stampDream writes the stamp; fails is the consecutive failure count (0 = success).
func stampDream(at time.Time, fails int) error {
	body := at.Format(time.RFC3339) + "\n"
	if fails > 0 {
		body += "failed " + strconv.Itoa(fails) + "\n"
	}
	return paths.AtomicWrite(dreamStampPath(), []byte(body), 0o600)
}

// Dream outcomes in a result record.
const (
	DreamOK     = "ok"
	DreamNoop   = "noop" // not due, or the run changed no bank file
	DreamFailed = "failed"
)

// DreamResult is one line of state/harness/memory/dream-results.jsonl, written for every dream run.
type DreamResult struct {
	At         time.Time `json:"at"`
	Trigger    string    `json:"trigger"` // session-start | schedule | manual
	Forced     bool      `json:"forced,omitempty"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason,omitempty"` // "not due", "no bank change", or the failure's first line
	Agent      string    `json:"agent,omitempty"`
	Profile    string    `json:"profile,omitempty"`
	Model      string    `json:"model,omitempty"`
	Local      bool      `json:"local,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Archived   int       `json:"archived"`
	Changed    []string  `json:"changed,omitempty"` // bank files the run modified, store-relative
	Log        string    `json:"log,omitempty"`
}

const dreamResultsKeep = 200 // newest lines kept; one line per run

func dreamResultsPath() string { return filepath.Join(paths.MemoryStateDir(), "dream-results.jsonl") }

// appendDreamResult adds r and keeps the newest dreamResultsKeep lines.
func appendDreamResult(r DreamResult) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := dreamResultsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(p+".lock", func() error {
		old, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		lines := append(strings.Split(strings.TrimSpace(string(old)), "\n"), string(line))
		if len(lines) > 1 && lines[0] == "" {
			lines = lines[1:]
		}
		if len(lines) > dreamResultsKeep {
			lines = lines[len(lines)-dreamResultsKeep:]
		}
		return paths.AtomicWrite(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	})
}

// LastDreamResults returns the newest n records, newest first; a missing file is none.
func LastDreamResults(n int) ([]DreamResult, error) {
	data, err := os.ReadFile(dreamResultsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var out []DreamResult
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		var r DreamResult
		if json.Unmarshal([]byte(lines[i]), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// dreamLogKeep is how many per-run dream logs stay in the state dir, the new one included.
const dreamLogKeep = 50

// openDreamLog creates the run's uniquely named log in the state dir after pruning the oldest ones.
func openDreamLog() (*os.File, error) {
	dir := paths.MemoryStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pruneDreamLogs(dir, dreamLogKeep-1)
	return os.CreateTemp(dir, "dream-"+time.Now().Format(logStampLayout)+"-*.log")
}

// pruneDreamLogs keeps the newest keep stamped dream logs (never dream-launchd.log); names sort oldest first.
func pruneDreamLogs(dir string, keep int) {
	logs, _ := filepath.Glob(filepath.Join(dir, "dream-[0-9]*.log"))
	sort.Strings(logs)
	for i := 0; i < len(logs)-keep; i++ {
		_ = os.Remove(logs[i])
	}
}
