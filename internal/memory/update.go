package memory

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// currentStateFiles are the bank files an older session must never touch.
var currentStateFiles = []string{"activeContext.md", "progress.md", "conventions.md"}

// snapshotFiles reads each of names in b; a missing file maps to nil.
func snapshotFiles(names []string) map[string][]byte {
	snap := make(map[string][]byte, len(names))
	for _, n := range names {
		snap[n], _ = os.ReadFile(Path(n))
	}
	return snap
}

// warnChanged names every file whose bytes differ from snap; it never restores (a live session may own the write).
func warnChanged(out io.Writer, snap map[string][]byte) {
	for _, n := range slices.Sorted(maps.Keys(snap)) {
		if now, _ := os.ReadFile(Path(n)); !bytes.Equal(now, snap[n]) {
			fmt.Fprintf(out, "memory update: WARNING: historical run changed %s (not restored)\n", n)
		}
	}
}

// Update statuses: pending = dispatched, not yet reported.
const (
	updatePending = "pending"
	updateOK      = "ok"
	updateFailed  = "failed"
	updateBlocked = "blocked" // a past update the removed permission gate refused; read from old records only
)

// updateGrace must exceed the worker's lock waits plus its own timeout, or a healthy queued update reads as lost.
var updateGrace = detachedLockWait + homeLockWait + updateTimeout + time.Minute

// updateLockWait is a foreground update's wait for a peer's .postupdate lock before giving up as busy; a var for tests.
var updateLockWait = 5 * time.Second

// detachedLockWait outlasts a dream (updateTimeout) so a detached session-end save queues, not lost; a var for tests.
var detachedLockWait = updateTimeout + time.Minute

// DetachedEnv marks a worker spawned by hooks or sweep (no one waits on it), so its update may queue for the lock.
const DetachedEnv = "CLAUDE_MEMORY_DETACHED"

// lockWait is detachedLockWait for a worker spawned detached, else updateLockWait.
func lockWait() time.Duration {
	if v := os.Getenv(DetachedEnv); v != "" && v != "0" {
		return detachedLockWait
	}
	return updateLockWait
}

// homeLockWait is how long a worker queues for the home bank's lock behind another repo's run; a var for tests.
var homeLockWait = 15 * time.Minute

// updateTimeout bounds the headless claude run.
const updateTimeout = 15 * time.Minute

// UpdateResult is one session's update outcome (KEY=value lines on disk).
type UpdateResult struct {
	Status          string
	Time            time.Time
	BankUpdated     bool
	Log             string
	Class, Fix, Ran string
	Local           bool
	Typed           string // "ok" when the op batch applied, "fallback" when the tools-on run did the work
}

func resultPath(sessionID string) string {
	return filepath.Join(receiptDir(), "update-result-"+filepath.Base(sessionID))
}

const receiptSubdir = "update-results"

// Retry legacy moves until none remain; never replace a receipt a writer has already published.
func receiptDir() string {
	state := paths.MemoryStateDir()
	dir := filepath.Join(state, receiptSubdir)
	marker := filepath.Join(dir, ".migrated")
	if _, err := os.Stat(marker); err == nil {
		return dir
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "memory update: inspect receipt migration marker: %v\n", err)
		return dir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return dir
	}
	for _, p := range transcript.GlobIn(state, "update-result-*") {
		dst := filepath.Join(dir, filepath.Base(p))
		if err := os.Link(p, dst); err != nil {
			if !errors.Is(err, os.ErrExist) {
				continue
			}
			// Finish an interrupted unlink, but preserve distinct files on a name collision.
			srcInfo, _ := os.Lstat(p)
			dstInfo, _ := os.Lstat(dst)
			if !os.SameFile(srcInfo, dstInfo) {
				continue
			}
		}
		_ = os.Remove(p)
	}
	if len(transcript.GlobIn(state, "update-result-*")) == 0 {
		if err := paths.AtomicWrite(marker, nil, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "memory update: mark receipt migration complete: %v\n", err)
		}
	}
	return dir
}

// writeUpdateResult persists r for sessionID; a blank id (manual run) records nothing.
func writeUpdateResult(sessionID string, r UpdateResult) error {
	if sessionID == "" {
		return nil
	}
	var body strings.Builder
	fmt.Fprintf(&body, "status=%s\ntime=%s\nbank_updated=%t\nlog=%s\n",
		r.Status, r.Time.Format(time.RFC3339Nano), r.BankUpdated, r.Log)
	for _, field := range []struct{ key, value string }{{"class", r.Class}, {"fix", r.Fix}, {"ran", r.Ran}, {"typed", r.Typed}} {
		if field.value != "" {
			fmt.Fprintf(&body, "%s=%s\n", field.key, field.value)
		}
	}
	if r.Local {
		body.WriteString("local=true\n")
	}
	return paths.AtomicWrite(resultPath(sessionID), []byte(body.String()), 0o600)
}

// readUpdateResult loads sessionID's result; ok=false when absent or unparseable.
func readUpdateResult(sessionID string) (UpdateResult, bool) {
	// Bulk readers already migrated the directory; don't rescan the state dir for each receipt.
	state := paths.MemoryStateDir()
	name := "update-result-" + filepath.Base(sessionID)
	data, err := os.ReadFile(filepath.Join(state, receiptSubdir, name))
	if errors.Is(err, os.ErrNotExist) {
		data, err = os.ReadFile(resultPath(sessionID))
	}
	if err != nil {
		return UpdateResult{}, false
	}
	m := map[string]string{}
	for ln := range strings.SplitSeq(string(data), "\n") {
		if k, v, ok := strings.Cut(ln, "="); ok {
			m[k] = v
		}
	}
	t, err := time.Parse(time.RFC3339, m["time"])
	if err != nil {
		return UpdateResult{}, false
	}
	r := UpdateResult{Status: m["status"], Time: t, Log: m["log"], BankUpdated: m["bank_updated"] == "true",
		Class: m["class"], Fix: m["fix"], Ran: m["ran"], Local: m["local"] == "true", Typed: m["typed"]}
	return r, r.Status != ""
}

// needsAttention: a recorded failure, or a pending dispatch past the grace window (the worker died).
func (r UpdateResult) needsAttention(now time.Time) bool {
	return r.Status == updateFailed || (r.Status == updatePending && now.Sub(r.Time) > updateGrace)
}

// AgentFunc runs prompt on an injected agent (internal/agent imports this package) and names who answered.
type AgentFunc func(ctx context.Context, prompt, workdir string) (who string, err error)

// agentUpdatePrompt frames `memory update` for an agent without slash commands; the stable procedure leads for caching.
func agentUpdatePrompt(transcript, session, cwd string, historical bool) (string, error) {
	ref := assets.MemoryUpdateProcedure()
	if historical {
		ref = dropCurrentStateSteps(ref)
	}
	return assets.MemoryUpdatePrompt(assets.MemoryUpdateInput{
		Worker: assets.MemoryUpdateWorker(bankPaths()), Procedure: ref, Historical: historicalBlock(historical),
		Digest: digestBlock(transcript), Git: gitSinceBlock(transcript, cwd), Tags: tagBlock(transcript, session, historical),
	}), nil
}

// bankPaths lists b's five typed file paths, comma-separated on one line.
func bankPaths() string {
	ps := make([]string, len(bankFiles))
	for i, n := range bankFiles {
		ps[i] = Path(n)
	}
	return strings.Join(ps, ", ")
}

// dropCurrentStateSteps removes the Step sections for activeContext, progress and conventions from ref.
func dropCurrentStateSteps(ref string) string {
	header, blocks := splitHeadingBlocks(ref)
	blocks = slices.DeleteFunc(blocks, func(blk string) bool {
		return strings.HasPrefix(blk, "## Step 2:") || strings.HasPrefix(blk, "## Step 3:") || strings.HasPrefix(blk, "## Step 5:")
	})
	return rejoin(header, blocks)
}

// digestBlock inlines the transcript's text-only tail (user/assistant turns); without one, the worker reads the file.
func digestBlock(path string) string {
	h, err := transcript.Handoff(path, "", "")
	_, rest, ok := strings.Cut(h, "<<<TRANSCRIPT\n")
	body, _, ok2 := strings.Cut(rest, "\nTRANSCRIPT>>>")
	if err != nil || !ok || !ok2 {
		return assets.MemoryTranscriptPointer(path)
	}
	return assets.MemorySessionDigest(path, body)
}

func historicalBlock(historical bool) string {
	if historical {
		return assets.MemoryHistoricalRule()
	}
	return ""
}

// firstTranscriptTimestamp is the session's first "timestamp" (scopes `git log --since`); zero when absent.
func firstTranscriptTimestamp(transcript string) (first time.Time) {
	scanTranscriptHead(transcript, 20, func(line []byte) bool { // the first records may be snapshots with no timestamp
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return false
		}
		t, err := time.Parse(time.RFC3339, rec.Timestamp)
		first = t
		return err == nil
	})
	return first
}

// scanTranscriptHead feeds up to n leading JSONL lines of path to visit, stopping when it returns true.
func scanTranscriptHead(path string, n int, visit func(line []byte) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for range n {
		line, err := r.ReadBytes('\n')
		if visit(line) || err != nil {
			return
		}
	}
}

// gitSinceBlock lists this session's commits and touched files; empty without a timestamp or git repo.
func gitSinceBlock(transcript, cwd string) string {
	since := firstTranscriptTimestamp(transcript)
	if since.IsZero() || cwd == "" {
		return ""
	}
	log, _ := gitOut(cwd, "log", "--since="+since.Format(time.RFC3339), "--name-only", "--format="+commitMark+"%h %s")
	commits, touched := splitCommitLog(log)
	status, _ := gitOut(cwd, "status", "--porcelain")
	if commits == "" && touched == "" && status == "" {
		return ""
	}
	files := strings.Join(mergeFileLists(touched, status), "\n")
	return textutil.CapRunes(assets.MemoryGitActivity(commits, files), gitBlockMaxRunes, "\n…(list truncated)\n")
}

// commitMark prefixes each commit line of the combined log so file lines can be told apart.
const commitMark = "\x01"

// splitCommitLog separates the combined `log --name-only` output into oneline commits and touched paths.
func splitCommitLog(log string) (commits, touched string) {
	var c, f []string
	for ln := range strings.SplitSeq(log, "\n") {
		if subject, ok := strings.CutPrefix(ln, commitMark); ok {
			c = append(c, subject)
		} else {
			f = append(f, ln)
		}
	}
	return strings.Join(c, "\n"), strings.Join(f, "\n")
}

// gitBlockMaxRunes keeps a huge working tree from pushing the worker prompt past the argv limit (agent.MaxPromptBytes).
const gitBlockMaxRunes = 8000

// mergeFileLists dedupes git log's --name-only paths with status --porcelain's (2-char code prefix stripped).
func mergeFileLists(nameOnly, porcelain string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if u, err := strconv.Unquote(p); strings.HasPrefix(p, `"`) && err == nil {
			p = u // git C-quotes odd names; Go's octal escapes are bytes, so UTF-8 survives
		}
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for ln := range strings.SplitSeq(nameOnly, "\n") {
		add(strings.TrimSpace(ln))
	}
	for ln := range strings.SplitSeq(porcelain, "\n") {
		ln = strings.TrimRight(ln, "\r") // the leading space is part of the "XY " code, so never trim it
		if len(ln) > 3 {
			_, p, renamed := strings.Cut(ln[3:], " -> ") // a rename prints "old -> new"; keep the new path
			if !renamed {
				p = ln[3:]
			}
			add(strings.TrimSpace(p))
		}
	}
	return out
}

// tagBlock files the session's tags (historical: DECISION only): typed events, then transcript tags they do not repeat.
func tagBlock(transcript, session string, historical bool) string {
	filed := filedDecisions() // dropped before any cap, so filed decisions never push a pending tag out
	isFiled := func(t tag) bool { return t.Kind == "DECISION" && filed(t.Text) }
	var legacy []tag
	if f, err := os.Open(transcript); err == nil {
		defer f.Close()
		legacy, _ = extractTagsExcept(f, isFiled)
	}
	if session == "" { // no id on the request: the transcript's stem is the session id for Claude transcripts
		session = strings.TrimSuffix(filepath.Base(transcript), ".jsonl")
	}
	typed := typedTags(session)
	if historical { // before the cap, so reserved slots never go to tags this mode drops
		notDecision := func(t tag) bool { return t.Kind != "DECISION" }
		typed, legacy = slices.DeleteFunc(typed, notDecision), slices.DeleteFunc(legacy, notDecision)
	}
	tags := mergeTags(slices.DeleteFunc(typed, isFiled), legacy)
	if len(tags) == 0 {
		return ""
	}
	var lines []string
	for _, t := range tags {
		lines = append(lines, t.Kind+": "+t.Text)
	}
	return assets.MemorySessionTags(lines)
}

// filedDecisions reports whether a decision's text already heads an entry in decisionLog.md or its archive.
func filedDecisions() func(text string) bool {
	logs := append([]string{Path("decisionLog.md")}, transcript.GlobIn(filepath.Join(paths.MemoryStoreDir(), "archive"), "decisionLog*.md")...)
	titles := map[string]bool{} // a struck (superseded) title still counts: replaying must not revive it
	for _, p := range logs {
		b, _ := readFile(p)
		for ln := range strings.SplitSeq(string(b), "\n") {
			if _, title, ok := strings.Cut(ln, " — "); ok && strings.HasPrefix(ln, "## ") {
				titles[normTitle(title)] = true
				head, _, _ := strings.Cut(title, " — ") // an entry filed as "decision — rationale" matches a bare tag
				titles[normTitle(head)] = true
			}
		}
	}
	return func(text string) bool { return titles[normTitle(text)] }
}

// normTitle is a decision title without its id marker, strike marks and spacing differences.
func normTitle(s string) string {
	s = strings.ReplaceAll(idMarker().ReplaceAllString(s, ""), "~~", "")
	return strings.Join(strings.Fields(s), " ")
}

// filedByGoBlock lists the session's DECISION events as decisionLog.md entries (date, title, id); uncapped.
func filedByGoBlock(session string) string {
	if session == "" {
		return ""
	}
	entries := decisionEntryLines()
	var out strings.Builder
	for _, e := range ReadTagEvents(session) {
		if line, ok := entries[normTitle(e.Text)]; ok && e.Event == "decision" {
			out.WriteString(line)
			delete(entries, normTitle(e.Text))
		}
	}
	if out.Len() == 0 {
		return ""
	}
	return assets.MemoryFiledByGo(out.String())
}

// decisionEntryLines maps each decisionLog.md title, and its part before " — ", to a "- date — title (id x)" line.
func decisionEntryLines() map[string]string {
	b, _ := readFile(Path("decisionLog.md"))
	entries := map[string]string{}
	for ln := range strings.SplitSeq(string(b), "\n") {
		_, title, ok := strings.Cut(ln, " — ")
		if !ok || !strings.HasPrefix(ln, "## ") || len(ln) < 13 {
			continue
		}
		id := ""
		if m := idMarker().FindStringSubmatch(ln); m != nil {
			id = m[1]
		}
		line := "- " + ln[3:13] + " — " + normTitle(title) + " (id " + id + ")\n"
		head, _, _ := strings.Cut(title, " — ")
		entries[normTitle(title)], entries[normTitle(head)] = line, line
	}
	return entries
}

// fileDecisionEvents writes each recorded DECISION event not yet in decisionLog.md; Go, not the model, owns the entry.
func fileDecisionEvents(session string, now time.Time) error {
	var errs []error
	for _, e := range ReadTagEvents(session) {
		if e.Event != "decision" {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			at = now
		}
		if _, err := rememberDecisionOnce(e.Text, at); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// typedTags is the session's recorded tag events as tags; an empty session reads none.
func typedTags(session string) []tag {
	if session == "" {
		return nil
	}
	var out []tag
	for _, e := range ReadTagEvents(session) {
		out = append(out, tag{Kind: strings.ToUpper(e.Event), Text: e.Text})
	}
	return out
}

// mergeTags is typed first, then each legacy tag no typed one repeats; over the cap the legacy ones go first.
func mergeTags(typed, legacy []tag) []tag {
	seen := map[tag]bool{}
	var out, extra []tag
	for _, t := range typed {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range legacy {
		if !seen[t] {
			seen[t] = true
			extra = append(extra, t)
		}
	}
	if reserve := min(len(extra), tagMaxCount/5); len(out) > tagMaxCount-reserve { // legacy-only tags keep some room
		out = out[len(out)-(tagMaxCount-reserve):]
	}
	if room := tagMaxCount - len(out); len(extra) > room {
		extra = extra[len(extra)-room:]
	}
	return append(out, extra...)
}

// updateOptions drive one background update.
type updateOptions struct {
	Run        AgentFunc // the injected agent runner (ask.MemoryRunner in production)
	Transcript string
	CWD        string
	Session    string
	Handoff    handoffClaim
	Historical bool          // an older session: the prompt forbids current-state files and a changed one is reported
	Timeout    time.Duration // 0 = updateTimeout
	Out        io.Writer
}

// update runs the headless narrative pass under the bank lock, settles the bank, and records the outcome.
func update(o updateOptions) error {
	o.Handoff = takeHandoffBlock(o.Session)
	defer cleanHandoff(o.Session, o.Handoff)
	o = updateDefaults(o)
	prior, _ := readUpdateResult(o.Session)
	var run updateRun
	record := func(status string, updated bool) error {
		return recordUpdateResult(o.Session, prior.Log, status, updated, &run)
	}
	if o.Historical {
		snap := snapshotFiles(currentStateFiles)
		defer warnChanged(o.Out, snap)
	}
	run.before = newestMtime()
	run.typedPass(o) // the model runs outside the store lock; only the store's Apply and the settle take it
	// Bounded wait: a peer update/dream may hold this lock for minutes; only a detached worker can afford to queue.
	ran, lockErr := paths.TryFileLock(lockPathOf()+".lock", lockWait(), func() error { return run.locked(o) })
	if !ran { // lock never taken: nothing ran, and settling unlocked would race the holder
		uncoverStamp(o.Session) // the dispatch stamped this work as covered; hand it back to the next trigger or sweep
		return errors.Join(fmt.Errorf("headless update: %w", cmp.Or(lockErr, errors.New("another update or dream holds the store lock; retry when it finishes"))), record(updateFailed, false))
	}
	auditUpdatePlans(o)
	if err := errors.Join(lockErr, run.runErr, run.settleErr); err != nil {
		return errors.Join(fmt.Errorf("headless update: %w", err), record(updateFailed, run.updated))
	}
	return record(updateOK, run.updated)
}

func auditUpdatePlans(o updateOptions) {
	if o.Historical || o.CWD == "" {
		return
	}
	top := GitToplevel(o.CWD)
	if top == "" {
		return
	}
	if _, err := os.Stat(plans.Home(top)); errors.Is(err, os.ErrNotExist) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	report, err := plans.Audit(ctx, top, plans.AuditOpts{})
	if len(report.Changed) > 0 || len(report.Reported) > 0 {
		err = errors.Join(err, report.WriteText(o.Out))
	}
	if err != nil {
		fmt.Fprintf(o.Out, "memory update: plans audit: %s\n", strings.Join(strings.Fields(err.Error()), " "))
	}
}

// FixSessionSlot in a runner's fix command is replaced with the session id when the receipt is written.
const FixSessionSlot = "<session-id>"

const budgetFixGuidance = "retry outside the spent dispatch tree, or after raising the cap"

// fixNote is the receipt's fix for the owner; a budget failure also says where a retry can run.
func (r UpdateResult) fixNote() string {
	if r.Fix == "" || r.Class != JobClassBudget || strings.Contains(r.Fix, "outside the spent dispatch tree") {
		return r.Fix
	}
	return r.Fix + " (" + budgetFixGuidance + ")"
}

// recordUpdateResult writes the session's outcome, keeping the dispatch's log path.
func recordUpdateResult(session, log, status string, updated bool, run *updateRun) error {
	rep, typed := run.report, run.typed
	ran := strings.Join([]string{rep.Agent, rep.Profile, rep.Model}, "·")
	if rep.Agent == "" && rep.Profile == "" && rep.Model == "" {
		ran = ""
	}
	err := writeUpdateResult(session, UpdateResult{Status: status, Time: time.Now(), BankUpdated: updated, Log: log,
		Class: rep.Class, Fix: strings.ReplaceAll(rep.Fix, FixSessionSlot, textutil.QuoteSH(session)), Ran: ran, Local: rep.Local, Typed: typed})
	if err != nil {
		return fmt.Errorf("record update result: %w", err)
	}
	return nil
}

// uncoverStamp resets a dispatch stamp ("bg") to zero covered edits, keeping its time so the Stop cooldown still holds.
func uncoverStamp(sessionID string) {
	if sessionID == "" {
		return
	}
	path := stampPath(sessionID)
	_ = paths.WithFileLock(path, func() error {
		if st, ok := readStamp(path); ok && st.state == "bg" && st.edits > 0 {
			return writeStamp(path, st.at, "bg", 0)
		}
		return nil
	})
}

func updateDefaults(o updateOptions) updateOptions {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = updateTimeout
	}
	return o
}

// updateRun is what the locked section of Update observed: bank mtimes before the run, the run's error, and settlement.
type updateRun struct {
	before            time.Time
	runErr, settleErr error
	updated           bool
	report            JobResult
	typed             string // "ok" or "fallback"; "" when the typed pass never ran
	fb                CodeFallback
}

// finish settles under the writer lock and reports whether the update wrote the bank.
func (r *updateRun) finish(o updateOptions) (updated bool, err error) {
	updated = r.touched()
	if _, err := settleBank(time.Now()); err != nil {
		return updated, fmt.Errorf("settle: %w", err)
	}
	// Refresh the cached doctor notice here (background worker), never on the SessionStart launch path.
	refreshNotice(o.Out, "memory update", o.CWD)
	return updated, nil
}

// touched reports whether the bank changed since the run began.
func (r *updateRun) touched() bool {
	return newestMtime().After(r.before)
}

// runAgent runs the agent, then completes the session's handoff when it succeeded.
func (r *updateRun) runAgent(ctx context.Context, o updateOptions) {
	r.runErr = runAgentUpdate(ctx, o)
	if r.runErr == nil && o.Handoff.block != "" {
		r.runErr = completeHandoff(o.Session, o.Handoff)
	}
}

// locked runs the agent update and its settlement under the store lock; errors ride in the run struct.
func (r *updateRun) locked(o updateOptions) error {
	if r.typed != "ok" && r.typed != "failed" {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		ctx, rep := WithJobReport(ctx)
		r.runAgent(ctx, o)
		r.report = *rep
	}
	r.updated, r.settleErr = r.finish(o)
	return nil
}

// runAgentUpdate hands the update prompt to o.Run and reports which agent ran it.
func runAgentUpdate(ctx context.Context, o updateOptions) error {
	prompt, err := agentUpdatePrompt(o.Transcript, o.Session, o.CWD, o.Historical)
	if err != nil {
		return err
	}
	prompt += filedByGoBlock(o.Session) + o.Handoff.block
	// Stops without an update worker launch a separate mint-only refresh.
	RefreshStaleEmbeddings(o.CWD)
	who, err := o.Run(withChildEnv(ctx, UpdateChildEnv+"=1"), prompt, o.CWD) // a fallback dispatch must not re-trigger hooks
	fmt.Fprintln(o.Out, "memory update: ran on", who)
	if err == nil && o.Session != "" {
		err = errors.Join(err, fileDecisionEvents(o.Session, time.Now()))
	}
	return err
}

// maxListedEntries and maxListedRunes cap what the model sees of each file and entry.
const (
	maxListedEntries = 60
	maxListedRunes   = 120
)

// typedPass runs the model outside the store lock and applies its op batch; no usable answer leaves typed "fallback".
func (r *updateRun) typedPass(o updateOptions) {
	if o.Handoff.block != "" { // a handoff needs a file the tools-off run cannot write
		r.typed = "fallback"
		return
	}
	listing, base, err := bankListing(o.Historical)
	if err != nil {
		r.typed = "fallback"
		r.runErr = err
		return
	}
	prompt, err := agentUpdatePrompt(o.Transcript, o.Session, o.CWD, o.Historical)
	if err != nil {
		r.typed = "fallback"
		return
	}
	head := prompt + filedByGoBlock(o.Session)
	prompt = head + listing
	b, ok := r.askBatch(o, prompt)
	if !ok {
		r.typed = "fallback"
		if r.fb.Used() { // the one code-mode dispatch is spent: fail, never the untyped writer
			r.typed = "failed"
			r.runErr = cmp.Or(r.runErr, errors.New("typed answer rejected and the code-mode fallback is spent"))
		}
		return
	}
	r.typed = "ok"
	r.runErr = r.report.named(r.applyAndFile(o, head, withTags(o, b), base))
	RefreshStaleEmbeddings(o.CWD)
}

// applyAndFile applies b (one fallback retry on a refusal), then files the session's decision events only on success.
func (r *updateRun) applyAndFile(o updateOptions, head string, b Batch, base Versions) error {
	err := applyTyped(o, b, base)
	if err != nil && refused(err) {
		err = r.retryApply(o, head, err)
	}
	if err == nil && o.Session != "" {
		err = fileDecisionEvents(o.Session, time.Now())
	}
	return err
}

// withTags puts the session's Go-filed tag ops ahead of the model's; an older session drops current-state targets.
func withTags(o updateOptions, b Batch) Batch {
	b.Ops = append(tagOps(o.Session, o.Historical), b.Ops...)
	if o.Historical {
		b.Ops = slices.DeleteFunc(b.Ops, func(op Op) bool { return slices.Contains(currentStateTargets, op.Target) })
	}
	return b
}

// currentStateTargets are the op targets an older session must never write.
var currentStateTargets = []string{"activeContext", "progress", "conventions"}

// askBatch runs the model with the batch schema, retrying once with the rejection message on an invalid answer.
func (r *updateRun) askBatch(o updateOptions, prompt string) (Batch, bool) {
	var rejected error
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		ctx, rep := WithJobReport(WithCodeFallback(ctx, &r.fb))
		ctx, answer := WithJobAnswer(ctx, BatchSchema())
		who, err := o.Run(withChildEnv(ctx, UpdateChildEnv+"=1"), prompt, o.CWD)
		cancel()
		r.report = *rep
		fmt.Fprintln(o.Out, "memory update: ran on", who)
		if err != nil {
			fmt.Fprintln(o.Out, "memory update: typed run failed:", err)
			r.runErr = err
			return Batch{}, false
		}
		b, err := decodeBatch(*answer)
		if err == nil {
			return b, true
		}
		rejected = err
		prompt += "\n\nYour previous answer was rejected: " + err.Error() + "\nReturn a corrected batch."
	}
	return r.fallbackBatch(o, prompt, rejected)
}

// fallbackBatch gives the one code-mode agent the exhausted validation failure; a spent fallback fails the job.
func (r *updateRun) fallbackBatch(o updateOptions, prompt string, rejected error) (b Batch, ok bool) {
	ctx, cancel := context.WithTimeout(withChildEnv(context.Background(), UpdateChildEnv+"=1"), o.Timeout)
	defer cancel()
	err := r.fb.Retry(ctx, prompt, o.CWD, BatchSchema(), r.report.models(), rejected, func(answer string) (derr error) {
		b, derr = decodeBatch(answer)
		return derr
	})
	if err != nil && r.fb.Used() {
		r.runErr = err
	}
	return b, err == nil
}

// decodeBatch parses the model's answer strictly and validates it.
func decodeBatch(answer string) (Batch, error) {
	if strings.TrimSpace(answer) == "" {
		return Batch{}, errors.New("no answer")
	}
	dec := json.NewDecoder(strings.NewReader(answer))
	dec.DisallowUnknownFields()
	var b Batch
	if err := dec.Decode(&b); err != nil {
		return Batch{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Batch{}, errors.New("expected one JSON object")
	}
	return b, b.Validate()
}

// applyTyped applies b with the refs the model was shown; failed ops are reported and nothing is written.
func applyTyped(o updateOptions, b Batch, base Versions) error {
	if err := refuseDerivedSupersede(b); err != nil {
		fmt.Fprintln(o.Out, "memory update:", err)
		return err
	}
	results, err := newBankStore(false).Apply(b, base, time.Now())
	for i, res := range results {
		if res.Outcome == DreamFailed {
			fmt.Fprintf(o.Out, "memory update: op %d (%s) failed: %s\n", i, res.Op.Op, res.Reason)
		}
	}
	return withFailedOps(err, results)
}

// withFailedOps appends each failed op's reason to err, so a fallback sees why the batch failed.
func withFailedOps(err error, results []OpResult) error {
	if err == nil {
		return nil
	}
	var failed []string
	for i, res := range results {
		if res.Outcome == DreamFailed {
			failed = append(failed, fmt.Sprintf("op %d (%s): %s", i, res.Op.Op, res.Reason))
		}
	}
	if len(failed) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.Join(failed, "; "))
}

// retryApply gives the one code-mode fallback the failed ops and a fresh listing, then applies its corrected batch.
func (r *updateRun) retryApply(o updateOptions, head string, failed error) error {
	listing, base, err := bankListing(o.Historical)
	if err != nil {
		return errors.Join(failed, err)
	}
	ctx, cancel := context.WithTimeout(withChildEnv(context.Background(), UpdateChildEnv+"=1"), o.Timeout)
	defer cancel()
	first := r.report.models()
	return r.fb.Retry(ctx, head+listing, o.CWD, BatchSchema(), first, failed, func(answer string) error {
		nb, err := decodeBatch(answer)
		if err != nil {
			return err
		}
		return applyTyped(o, withTags(o, nb), base)
	})
}

// tagOps files the session's NEXT, BUG and NOTE events without the model; invalid ones are dropped.
func tagOps(session string, historical bool) []Op {
	var ops []Op
	for _, t := range typedTags(session) {
		var op Op
		switch t.Kind {
		case "NEXT":
			op = Op{Op: "add", Target: "activeContext", Section: "Next Steps", Text: t.Text}
		case "BUG":
			op = Op{Op: "add", Target: "progress", Section: "Known Issues", Text: t.Text}
		case "NOTE":
			op = Op{Op: "add", Target: "conventions", Text: t.Text} // "note" is no bank file: the store refused the whole batch
		default:
			continue
		}
		if historical && slices.Contains(currentStateTargets, op.Target) {
			continue
		}
		if op.check() == nil {
			ops = append(ops, op)
		}
	}
	return ops
}

// bankListing shows each bank entry as "ref · first line" and returns the store's versions as the apply base.
func bankListing(historical bool) (string, Versions, error) {
	docs, base, err := newBankStore(false).Load()
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	sb.WriteString("\n\nBank entries (ref · first line); an op's id is one of these refs:\n")
	for _, name := range bankFiles {
		d := docs[name]
		if historical && slices.Contains(currentStateTargets, d.Name) {
			continue
		}
		fmt.Fprintf(&sb, "\n%s:\n", d.Name)
		for _, e := range d.Entries[:min(len(d.Entries), maxListedEntries)] {
			fmt.Fprintf(&sb, "- %s · %s\n", e.Ref(), textutil.CapRunes(e.Lines[0], maxListedRunes, "…"))
		}
	}
	return sb.String(), base, nil
}

type childEnvKey struct{}

// withChildEnv adds KEY=VALUE markers the AgentFunc's child process must carry; the parent env stays untouched.
func withChildEnv(ctx context.Context, env ...string) context.Context {
	return context.WithValue(ctx, childEnvKey{}, env)
}

// ChildEnv returns the markers the run's child process must carry, for an AgentFunc to layer onto its exec env.
func ChildEnv(ctx context.Context) []string {
	env, _ := ctx.Value(childEnvKey{}).([]string)
	return env
}

// UpdateChildEnv marks a spawned update worker so its own session hooks never schedule another.
const UpdateChildEnv = "CLAUDE_SESSION_UPDATE_CHILD"

// IsUpdateChild reports whether this process runs inside an update worker.
func IsUpdateChild() bool {
	v := os.Getenv(UpdateChildEnv)
	return v != "" && v != "0"
}

// tag is one DECISION/BUG/NEXT/NOTE line the assistant wrote in a reply.
type tag struct {
	Kind string
	Text string
}

// tagMaxLen caps a single tag; tagMaxCount keeps only the newest tags (dedup preserves first-seen order).
const (
	tagMaxLen   = 300
	tagMaxCount = 40
)

var tagKinds = []string{"DECISION:", "BUG:", "NEXT:", "NOTE:"}

// extractTagsExcept returns ASSISTANT-only tag lines minus skip, dropped before the cap so none evicts a pending one.
func extractTagsExcept(r io.Reader, skip func(tag) bool) ([]tag, error) {
	var tags []tag
	seen := map[string]bool{}
	err := transcript.EachLine(r, func(line []byte) {
		for _, t := range tagsFromLine(line) {
			if skip != nil && skip(t) {
				continue
			}
			key := t.Kind + "\x00" + t.Text
			if !seen[key] {
				seen[key] = true
				tags = append(tags, t)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if len(tags) > tagMaxCount {
		tags = tags[len(tags)-tagMaxCount:]
	}
	return tags, nil
}

// tagsFromLine parses one JSONL record and extracts tags from its assistant text blocks, if any.
func tagsFromLine(line []byte) []tag {
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &rec) != nil {
		return nil
	}
	if rec.Type != "assistant" || rec.Message.Role != "assistant" {
		return nil
	}
	var tags []tag
	for _, block := range rec.Message.Content {
		if block.Type != "text" {
			continue
		}
		for ln := range strings.SplitSeq(block.Text, "\n") {
			if t, ok := parseTagLine(ln); ok {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// parseTagLine matches an optional "- "/"* " bullet then a DECISION:/BUG:/NEXT:/NOTE: prefix.
func parseTagLine(ln string) (tag, bool) {
	ln = strings.TrimSpace(ln)
	ln = strings.TrimPrefix(strings.TrimPrefix(ln, "- "), "* ")
	for _, kind := range tagKinds {
		if after, ok := strings.CutPrefix(ln, kind); ok {
			text := strings.TrimSpace(after)
			if len(text) > tagMaxLen {
				text = strings.ToValidUTF8(text[:tagMaxLen], "") // a cut mid-rune leaves a partial one
			}
			return tag{Kind: strings.TrimSuffix(kind, ":"), Text: text}, true
		}
	}
	return tag{}, false
}

// ParseTagLine is parseTagLine for the Stop hook: the tag's kind ("DECISION"...) and text.
func ParseTagLine(ln string) (kind, text string, ok bool) {
	t, ok := parseTagLine(ln)
	return t.Kind, t.Text, ok
}

// TagEvent is one typed session event the Stop hook recorded from an assistant tag line.
type TagEvent struct {
	Event string `json:"event"`
	Text  string `json:"text"`
	At    string `json:"at"`
}

// TagEventsPath is the session's append-only tag event log, one TagEvent JSON per line.
func TagEventsPath(session string) string {
	return filepath.Join(paths.MemoryStateDir(), "tags-"+filepath.Base(session)+".jsonl")
}

// ReadTagEvents returns the session's recorded events in order; a missing log or a torn line yields what parses.
func ReadTagEvents(session string) []TagEvent {
	b, err := os.ReadFile(TagEventsPath(session))
	if err != nil {
		return nil
	}
	var out []TagEvent
	for ln := range bytes.SplitSeq(b, []byte("\n")) {
		var e TagEvent
		if json.Unmarshal(ln, &e) == nil && e.Event != "" && e.Text != "" {
			out = append(out, e)
		}
	}
	return out
}

// handoffMarker is the per-session file the context steward leaves for its update worker; it holds the handoff path.
func handoffMarker(sessionID string) string {
	return filepath.Join(paths.MemoryStateDir(), "handoff-"+filepath.Base(sessionID))
}

// handoffRunMarker stands in for the pending marker while the worker writes the handoff.
func handoffRunMarker(sessionID string) string {
	return filepath.Join(paths.MemoryStateDir(), "handoff-run-"+filepath.Base(sessionID))
}

// stewardHandoffPath is the one path the steward's handoff for this session is written to.
func stewardHandoffPath(sessionID string, now time.Time) string {
	return paths.MemorySub("handoffs", now.Format("2006-01-02")+"-session-"+compactID(sessionID)+".md")
}

// JobDream names the dream job in plan rechecks.
const JobDream = "dream"

// Failure classes a runner reports; auth and budget need the owner, so no automatic retry repeats them.
const (
	JobClassAuth   = "auth"
	JobClassBudget = "budget"
	JobClassQuota  = "quota"
	JobClassFailed = "failed"
)

// JobResult is what the injected runner says about its run: the route that answered, a failure's class and fix.
type JobResult struct {
	Agent, Profile, Model, Primary, Class, Fix string
	Local                                      bool // a local-model run: lower trust, same caps and validation
}

func (j JobResult) models() string {
	model := cmp.Or(j.Model, j.Agent)
	if j.Primary != "" {
		return j.Primary + " then " + model
	}
	return model
}

// named prefixes a post-commit failure (never retried, so no fallback line names the models) with who answered.
func (j JobResult) named(err error) error {
	if err == nil || refused(err) || j.models() == "" {
		return err
	}
	return fmt.Errorf("%s: %w", j.models(), err)
}

type jobReportKey struct{}

// WithJobReport gives the runner a report slot; the job reads it after o.Run returns.
func WithJobReport(ctx context.Context) (context.Context, *JobResult) {
	r := &JobResult{}
	return context.WithValue(ctx, jobReportKey{}, r), r
}

// ReportJob fills ctx's report slot; a ctx without one (a test runner) drops it.
func ReportJob(ctx context.Context, r JobResult) {
	if p, ok := ctx.Value(jobReportKey{}).(*JobResult); ok {
		*p = r
	}
}

type jobOutputKey struct{}

// WithJobOutput routes the runner's agent output, receipts and notes to w (the job's log tee).
func WithJobOutput(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, jobOutputKey{}, w)
}

// JobOutput is the writer set by WithJobOutput, else fallback (the runner's own stdout or stderr).
func JobOutput(ctx context.Context, fallback io.Writer) io.Writer {
	if w, ok := ctx.Value(jobOutputKey{}).(io.Writer); ok && w != nil {
		return w
	}
	return fallback
}

type jobAnswerKey struct{}

type jobAnswer struct {
	schema string
	text   *string
}

// WithJobAnswer asks the runner for one answer matching schema; the job reads *text after o.Run returns.
func WithJobAnswer(ctx context.Context, schema string) (context.Context, *string) {
	text := new(string)
	return context.WithValue(ctx, jobAnswerKey{}, jobAnswer{schema, text}), text
}

// JobAnswerSchema is the schema ctx asks for; "" means a tools-on run with no structured answer.
func JobAnswerSchema(ctx context.Context) string {
	a, _ := ctx.Value(jobAnswerKey{}).(jobAnswer)
	return a.schema
}

// AnswerJob fills ctx's answer slot; a ctx without one (a test runner) drops it.
func AnswerJob(ctx context.Context, text string) {
	if a, ok := ctx.Value(jobAnswerKey{}).(jobAnswer); ok {
		*a.text = text
	}
}

// refuseDerivedSupersede refuses superseding an entry with no stored id: striking it changes its ref.
func refuseDerivedSupersede(b Batch) error {
	for i, o := range b.Ops {
		if r, _ := ParseRef(o.ID); o.Op == "supersede" && r.Derived {
			return fmt.Errorf("op %d (supersede): %s has no stored id, so the link would dangle; nothing written", i, o.ID)
		}
	}
	return nil
}
