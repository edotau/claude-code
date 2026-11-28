package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/review/bench"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/routing/providers"
)

var idRefRE = regexp.MustCompile(`^(z[0-9a-f]{8}|[0-9a-f]{8}(-[a-z0-9-]+)?)$`)

// idRef is a block reference: a persisted 8-hex id (or id-alias) or a z-prefixed contentRef.
func idRef() *regexp.Regexp { return idRefRE }

// SearchOptions is one `memory search`; Expand lets a lone block reference resolve to its block.
type SearchOptions struct {
	Query  string
	K      int
	Expand bool
}

// searchResult is the expanded block (Path, Block) when the query was a reference, else the ranked Hits.
type searchResult struct {
	Path, Block string
	Hits        []SearchHit
}

// Search ranks blocks for the query and logs the access; a block reference expands in place when allowed.
func Search(o SearchOptions) searchResult {
	now := time.Now()
	if o.Expand && idRef().MatchString(o.Query) {
		if path, block, err := ShowBlock(o.Query); err == nil {
			logShown(o.Query, AccessSearch, now)
			return searchResult{Path: path, Block: block}
		}
	}
	stop := RecallStage("search")
	hits := SearchBank(o.Query, o.K)
	stop()
	logHits(hits, AccessSearch, now, "")
	return searchResult{Hits: hits}
}

// Show returns one block by id and logs the access.
func Show(id string) (path, block string, err error) {
	path, block, err = ShowBlock(id)
	if err != nil {
		return "", "", err
	}
	logShown(id, AccessShow, time.Now())
	return path, block, nil
}

// DispatchCheckpoint serializes a checkpoint with other dispatches; an existing steward covers the request.
func DispatchCheckpoint(r UpdateRequest, handoff bool, now time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return DispatchCheckpointContext(ctx, r, handoff, now)
}

// DispatchCheckpointContext bounds stamp-lock contention by the caller's hook budget.
func DispatchCheckpointContext(ctx context.Context, r UpdateRequest, handoff bool, now time.Time) bool {
	covered := false
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return false
	}
	for ctx.Err() == nil {
		ran, err := paths.TryFileLock(stampPath(r.SessionID)+".lock", 0, func() error {
			covered = dispatchCheckpointLocked(r, handoff, now)
			return nil
		})
		if ran || err != nil {
			return covered && err == nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false
}

func dispatchCheckpointLocked(r UpdateRequest, handoff bool, now time.Time) bool {
	if StewardRunning(r.SessionID, time.Now()) {
		return true
	}
	if handoff {
		if err := markHandoff(r.SessionID, now); err != nil {
			fmt.Fprintln(os.Stderr, "context-checkpoint: handoff request:", err)
			return false
		}
	}
	started := dispatchUpdate(r, TriggerStop, countSessionEdits(r.SessionID, r.Transcript), now)
	if handoff && !started {
		_ = os.Remove(handoffMarker(r.SessionID))
	}
	return started
}

// Pending workers must start promptly; claimed workers may queue behind a dream before their agent runs.
const handoffPendingFresh = 2 * time.Minute

func stewardSince(sessionID string, now time.Time) time.Time {
	for i, p := range []string{handoffMarker(sessionID), handoffRunMarker(sessionID)} {
		bound := handoffPendingFresh
		if i == 1 {
			bound = detachedLockWait + updateTimeout + time.Minute
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.HasPrefix(string(b), sessionID+"\n") {
			continue
		}
		if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) < bound {
			return fi.ModTime()
		}
	}
	return time.Time{}
}

// StewardRunning reports whether this session's steward handoff job is dispatched and not finished.
func StewardRunning(sessionID string, now time.Time) bool {
	return !stewardSince(sessionID, now).IsZero()
}

// StewardHandoff returns only a completed handoff bearing this session's full identity.
func StewardHandoff(sessionID string, now time.Time) string {
	if sessionID == "" || StewardRunning(sessionID, now) {
		return ""
	}
	b, err := os.ReadFile(handoffDoneMarker(sessionID))
	fields := strings.SplitN(string(b), "\n", 3)
	if err != nil || len(fields) != 3 || fields[0] != sessionID {
		return ""
	}
	p := fields[1]
	if fi, err := os.Stat(p); err != nil || now.Sub(fi.ModTime()) > 24*time.Hour || !handoffOwned(p, sessionID) {
		return ""
	}
	return p
}

func handoffOwned(path, sessionID string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.HasPrefix(string(b), handoffIdentity(sessionID)+"\n")
}

func handoffDoneMarker(sessionID string) string { return handoffMarker(sessionID) + ".done" }

func handoffIdentity(sessionID string) string { return "<!-- session: " + sessionID + " -->" }

func markHandoff(sessionID string, now time.Time) error {
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return err
	}
	if err := os.Remove(handoffDoneMarker(sessionID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	data := sessionID + "\n" + stewardHandoffPath(sessionID, now) + "\n" + now.Format(time.RFC3339Nano)
	return paths.AtomicWrite(handoffMarker(sessionID), []byte(data), 0o600)
}

type handoffClaim struct{ block, request string }

// takeHandoffBlock claims one request for the worker's entire retry loop, including lock failures.
func takeHandoffBlock(sessionID string) handoffClaim {
	if sessionID == "" {
		return handoffClaim{}
	}
	b, err := os.ReadFile(handoffMarker(sessionID))
	fields := strings.SplitN(string(b), "\n", 3)
	if err != nil || len(fields) != 3 || fields[0] != sessionID {
		return handoffClaim{}
	}
	if err := os.Rename(handoffMarker(sessionID), handoffRunMarker(sessionID)); err != nil {
		return handoffClaim{}
	}
	now := time.Now()
	if err := os.Chtimes(handoffRunMarker(sessionID), now, now); err != nil {
		_ = os.Remove(handoffRunMarker(sessionID))
		return handoffClaim{}
	}
	return handoffClaim{request: string(b), block: assets.MemoryHandoffRequest(fields[1], handoffIdentity(sessionID))}
}

func cleanHandoff(sessionID string, claim handoffClaim) {
	b, err := os.ReadFile(handoffRunMarker(sessionID))
	if err == nil && claim.request != "" && string(b) == claim.request {
		if err := os.Remove(handoffRunMarker(sessionID)); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "memory update: handoff cleanup:", err)
		}
	}
}

func completeHandoff(sessionID string, claim handoffClaim) error {
	b, err := os.ReadFile(handoffRunMarker(sessionID))
	fields := strings.SplitN(string(b), "\n", 3)
	if err != nil {
		return err
	}
	if string(b) != claim.request || len(fields) != 3 || fields[0] != sessionID || !handoffOwned(fields[1], sessionID) {
		return errors.New("steward handoff missing or owned by another session")
	}
	return paths.AtomicWrite(handoffDoneMarker(sessionID), b, 0o600)
}

// StewardStale reports that the session's newest steward handoff predates its last compaction.
func StewardStale(sessionID string, now time.Time) bool {
	p := StewardHandoff(sessionID, now)
	fi, err := os.Stat(p)
	return err != nil || fi.ModTime().Before(lastCompactAt(sessionID))
}

// WaitStewardHandoff waits until the running steward job has written its file, or ctx ends; "" on a timeout.
func WaitStewardHandoff(ctx context.Context, sessionID string) string {
	for {
		now := time.Now()
		since := stewardSince(sessionID, now)
		p := StewardHandoff(sessionID, now)
		if since.IsZero() {
			return p
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Report appends a report to the bank's reports/ under the bank lock.
func Report(label string, in ReportInput) ([]string, error) {
	if err := EnsureBank(""); err != nil {
		return nil, err
	}
	return appendReport(label, in)
}

// InitBank creates the bank and reports where it landed, with the init procedure on a fresh template.
func InitBank(w io.Writer) error {
	dir, err := Init()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "bank:    %s\nhome:    %s\nsession-start injects it; `claude-code memory update` (or the background update) writes it.\n", dir, paths.MemorySubDir(""))
	if isBankTemplate() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, assets.MemoryInitProcedure())
	}
	return nil
}

// IndexOptions is one `memory index`.
type IndexOptions struct {
	Reembed  bool
	MintOnly bool
	DryRun   bool // print what MEMORY.md would hold and what folds; write nothing
}

// Index settles the bank (unless MintOnly) and refreshes the embedding cache; errors carry their verb prefix.
func Index(out, errw io.Writer, o IndexOptions) error {
	if o.DryRun {
		rep, err := refreshNativeIndex(time.Now(), true, nil)
		if err != nil {
			return fmt.Errorf("memory index: %w", err)
		}
		fmt.Fprintln(out, "dry run:", rep)
		return nil
	}
	root := root("")
	ctx := context.Background()
	profile, _ := embedEndpoint(ctx) // none usable: the session's profile, else the default
	profile = cmp.Or(profile, providers.SessionProfileContext(ctx))
	if o.Reembed {
		if err := reembedGate(time.Now()); err != nil {
			return err
		}
	}
	if o.MintOnly {
		embedIndex(ctx, out, errw, root, profile, o.Reembed)
		return nil
	}
	hasStore := bankExists()
	var rotated int
	err := withBankLock(BankLockWait, func() error {
		var serr error
		rotated, serr = settleBankLocked(time.Now())
		return serr
	})
	if err != nil {
		return fmt.Errorf("memory index: %w", err)
	}
	if hasStore {
		fmt.Fprintf(out, "indexed %s", paths.MemorySubDir(""))
		if rotated > 0 {
			fmt.Fprintf(out, " (rotated %d sessionHistory entries to archive/)", rotated)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintf(out, "no store at %s; run `claude-code memory init` in the repo to create one\n", paths.MemorySubDir(""))
	}
	refreshNotice(errw, "memory index", root)
	learnRecallForIndex(errw)
	embedIndex(ctx, out, errw, root, profile, o.Reembed)
	return nil
}

// reembedGate refuses --reembed before the override window opens.
func reembedGate(now time.Time) error {
	at, open, err := reembedWindow(now)
	if err != nil {
		return fmt.Errorf("memory index --reembed: %w", err)
	}
	if !open {
		return fmt.Errorf("memory index --reembed: embeddings are valid; the override opens at %s", at.Format("15:04"))
	}
	return nil
}

// embedIndex refreshes the entry-vector cache after an index; a missing endpoint or credential is a stderr note only.
func embedIndex(ctx context.Context, out, errw io.Writer, root, profile string, reembed bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	c, err := openEmbedClient(ctx, profile, "")
	if err == nil {
		var n, total int
		embed := embedChanged
		if reembed {
			embed = reembedEntries
		}
		if n, total, err = embed(ctx, root, c); err == nil && n > 0 {
			fmt.Fprintf(out, "embedded %d of %d entries (%s)\n", n, total, c.model)
		}
	}
	if err != nil && !errors.Is(err, errEmbedValid) { // a valid cache is the normal case, not a skip
		fmt.Fprintf(errw, "memory index: embeddings skipped: %v\n", err)
	}
}

// UpdateSessionOptions is one `memory update`; Run is only called once an update will really run.
type UpdateSessionOptions struct {
	Transcript, CWD, Session string
	DryRun, Historical       bool
	Run                      AgentFunc
	Out                      io.Writer
}

// resolveUpdateTarget resolves the bank root, session id and transcript an update acts on (transcript may be empty).
func resolveUpdateTarget(o UpdateSessionOptions) (root, session, transcript string, err error) {
	root = o.CWD
	if root == "" {
		if root, err = os.Getwd(); err != nil {
			return "", "", "", err
		}
	}
	session, transcript = o.Session, o.Transcript
	explicit := session != "" || transcript != ""
	if transcript == "" && session == "" { // a live session updates itself
		session = os.Getenv(paths.SessionIDEnv)
	}
	if transcript == "" {
		transcript = LiveTranscript(root, session)
	}
	if explicit && o.CWD == "" { // an explicit session's bank follows its project, not our cwd
		root = cmp.Or(sessionRoot(transcript), root)
	}
	return root, session, transcript, nil
}

// UpdateSession resolves the live session or transcript, then updates the bank (or prints the procedure / dry run).
func UpdateSession(o UpdateSessionOptions) error {
	root, session, transcript, err := resolveUpdateTarget(o)
	if err != nil {
		return err
	}
	if transcript == "" { // no resolvable session: print the narrative procedure instead of refusing outright
		ref := assets.MemoryUpdateProcedure()
		fmt.Fprintln(o.Out, ref)
		return nil
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "memory update (dry run): session %s, transcript %s, bank %s\n", session, transcript, paths.MemorySubDir(""))
		return nil
	}
	return update(updateOptions{run: o.Run, transcript: transcript, cwd: root, session: session, historical: o.Historical, out: o.Out})
}

// DreamRunOptions is one `memory update --dream`.
type DreamRunOptions struct {
	CWD           string
	Force, DryRun bool
	Run           AgentFunc
	Out           io.Writer
	Trigger       string
}

// Dream triggers: what started a run.
const (
	DreamTriggerManual       = "manual"
	DreamTriggerSessionStart = "session-start"
	DreamTriggerSchedule     = "schedule"
)

// dreamCatchup saves missed sessions before consolidating; a sweep already running (or this dream's parent) skips.
func dreamCatchup(out io.Writer) {
	_ = os.MkdirAll(paths.MemoryStateDir(), 0o700)
	lock := filepath.Join(paths.MemoryStateDir(), "catchup.lock")
	ran, err := paths.TryFileLock(lock, 0, func() error { runSweep("", time.Now(), out); return nil })
	if err != nil || !ran {
		fmt.Fprintln(out, "memory dream: catch-up skipped (a sweep holds the lock)")
	}
}

// RunDream previews (DryRun), catches up, then consolidates (or records not-due).
func RunDream(o DreamRunOptions) error {
	root := o.CWD
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	if o.DryRun {
		fmt.Fprint(o.Out, previewDream().report())
		return nil
	}
	d := dreamOptions{cwd: root, run: o.Run, out: o.Out, ifDue: !o.Force, trigger: cmp.Or(o.Trigger, DreamTriggerManual), forced: o.Force}.withLog()
	if o.Force || dreamDue(time.Now()) {
		dreamCatchup(d.out)
	}
	return dream(d)
}

// FormatReport wraps the bank files (or only lists them under check) and reports whether check found changes.
func FormatReport(w io.Writer, check bool) (dirty bool, err error) {
	changed, err := formatBank(check)
	if err != nil {
		return false, err
	}
	for _, f := range changed {
		fmt.Fprintln(w, f)
	}
	if check {
		if len(changed) > 0 {
			fmt.Fprintf(w, "%d file(s) would change (run `claude-code memory fmt`)\n", len(changed))
			return true, nil
		}
		fmt.Fprintln(w, "memory fmt --check: clean")
		return false, nil
	}
	fmt.Fprintf(w, "wrapped %d file(s)\n", len(changed))
	return false, nil
}

// FormatOwn wraps the bank under the lock, commits the change and lists what it wrapped.
func FormatOwn(w io.Writer) error {
	changed, err := formatBank(false)
	for _, f := range changed {
		fmt.Fprintln(w, f)
	}
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		fmt.Fprintf(w, "memory fmt: wrapped %d file(s)\n", len(changed))
	}
	return nil
}

// FormatCheck is FormatReport under check; cli calls it until it calls FormatReport(w, true) directly.
func FormatCheck(w io.Writer) (dirty bool, err error) { return FormatReport(w, true) }

// ReviewOptions is one `memory review`.
type ReviewOptions struct {
	StaleDays                 int
	Brief, Notice, candidates bool
	Candidate, Enforced       string // a candidate id; Enforced is the sha or file:line that closes it
	Approve                   bool
	Propose                   string // one candidate record as JSON; Go validates and appends it
}

// ReviewReport writes doctor + stats (Notice refreshes the cached session-start notice and prints nothing).
func ReviewReport(w io.Writer, o ReviewOptions) error {
	if o.candidateVerb() {
		return candidateAction(w, o)
	}
	root := root("")
	if o.Notice {
		return cacheDoctorNotice(doctor(root, o.StaleDays, time.Now()))
	}
	problems := doctor(root, o.StaleDays, time.Now())
	if err := cacheDoctorNotice(problems); err != nil {
		fmt.Fprintln(w, "memory review: doctor notice:", err)
	}
	problems = append(problems, reviseBankProblems()...)
	switch {
	case len(problems) == 0:
		fmt.Fprintln(w, "memory review: no problems found")
	case o.Brief:
		fmt.Fprintf(w, "memory review: %d problem(s) found\n", len(problems))
	default:
		for _, p := range problems {
			fmt.Fprintln(w, "- "+p)
		}
	}
	fmt.Fprint(w, collectBankStats().report())
	if !o.Brief && !o.candidates {
		writeOpenCandidates(w)
		writeHookSignals(w, time.Now())
	}
	if o.candidates {
		ref := assets.MemoryReviewProcedure()
		fmt.Fprintln(w)
		fmt.Fprintln(w, ref)
	}
	return candidateError(problems)
}

// TimelineReport writes the n most recent bank entries, then the sessionHistory lines.
func TimelineReport(w io.Writer, n int) error {
	entries, sessions, err := timelineBank(n)
	if err != nil {
		return fmt.Errorf("sessionHistory: %w", err)
	}
	for _, e := range entries {
		fmt.Fprintf(w, "%s  update   %s\n", e.time.Format("2006-01-02 15:04"), e.line)
	}
	if len(sessions) > 0 {
		fmt.Fprintln(w, "-- sessionHistory --")
		for _, s := range sessions {
			fmt.Fprintln(w, "- "+s)
		}
	}
	return nil
}

// dreamPreview is `memory update --dream --dry-run`'s deterministic report of the mechanical steps only.
type dreamPreview struct {
	dir           string
	oversized     []string // bank files over cap
	archivable    int      // durable-file overflow blocks that would move to this store's archive/
	sessionRotate int      // sessionHistory entries that would rotate to archive/
	unreadable    []string // "name: error" for each bank file that could not be read, so was not checked
	hookP95       []dreamBenchEntry
	hookP95Err    error
	rtkMisses     []rtkMiss
	rtkErr        error
}

// any reports whether the preview found anything a real dream run would change mechanically.
func (p dreamPreview) any() bool {
	return len(p.oversized) > 0 || p.archivable > 0 || p.sessionRotate > 0 || len(p.hookP95) > 0
}

// report renders the preview as `memory update --dream --dry-run`'s stdout.
func (p dreamPreview) report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bank: %s\n", p.dir)
	if len(p.oversized) > 0 {
		fmt.Fprintf(&b, "over cap: %s\n", strings.Join(p.oversized, ", "))
	}
	if p.archivable > 0 {
		fmt.Fprintf(&b, "would archive %d block(s)\n", p.archivable)
	}
	if p.sessionRotate > 0 {
		fmt.Fprintf(&b, "would rotate %d sessionHistory entr(ies) to archive/\n", p.sessionRotate)
	}
	for _, u := range p.unreadable {
		fmt.Fprintf(&b, "home %s — unreadable, so not checked\n", u)
	}
	writeDreamP95(&b, p.hookP95, p.hookP95Err)
	writeDreamRtk(&b, p.rtkMisses, p.rtkErr)
	if !p.any() && len(p.unreadable) == 0 {
		b.WriteString("nothing to consolidate mechanically — a real run still applies the judgment pass (dedup, merge, cross-pollinate)\n")
	}
	return b.String()
}

// previewDream computes dreamPreview without writing anything.
func previewDream() dreamPreview {
	var p dreamPreview
	// the telemetry scan and the rtk spawn are independent of the bank reads, so they run alongside them
	parallel.Rethrow(parallel.All(2, func(i int) error {
		if i == 1 {
			p.hookP95, p.hookP95Err = readDreamP95(filepath.Join(paths.StateDir(), "hooks"), time.Now())
			p.rtkMisses, p.rtkErr = readDreamRtk()
			return nil
		}
		local := statBank() // one read per typed file feeds the cap, archive and rotation checks below
		p.dir, p.oversized, p.unreadable = paths.MemorySubDir(""), local.oversized, local.unreadable
		for name, capLines := range archiveCaps {
			_, _, overflow := overflowSplit(name, string(local.data[name]), capLines) // archiveFile's split, read-only
			p.archivable += len(overflow)
		}
		_, _, rotate := sessionOverflow(string(local.data["sessionHistory.md"]))
		p.sessionRotate = len(rotate)
		return nil
	})...)
	return p
}

const dreamRotationSize = 2

type dreamBenchState struct {
	RankedAt time.Time `json:"ranked_at"`
	Ranking  []string  `json:"ranking"`
	Last     string    `json:"last"`
}

type dreamGoPackage struct {
	Dir          string
	ImportPath   string
	Imports      []string
	TestGoFiles  []string
	XTestGoFiles []string
}

func dreamBenchRotation(out io.Writer) error {
	root := paths.ConfigDir()
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("skipped: harness source: %w", err)
	}
	dir := filepath.Join(paths.StateDir(), "bench")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, "rotation.lock"), 0, func() error {
		return rotateDreamBench(root, filepath.Join(dir, "rotation.json"), out)
	})
	if !ran && err == nil {
		fmt.Fprintln(out, "memory dream: rotation: skipped (already running)")
	}
	return err
}

func rotateDreamBench(root, file string, out io.Writer) error {
	state, err := loadDreamBenchRanking(root, file)
	if err != nil {
		return err
	}
	selected := nextDreamBenchPackages(state.Ranking, state.Last)
	if len(selected) == 0 {
		fmt.Fprintln(out, "memory dream: rotation: skipped (no benchmark packages)")
	}
	var errs []error
	for _, pkg := range selected {
		if err := dreamBenchIdle(); err != nil {
			dreamStepError(out, "rotation: skipped "+pkg, err)
			break
		}
		err := runDreamBench(root, pkg, out)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", pkg, err))
		}
		state.Last = pkg
		if err := saveDreamBenchState(file, state); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

func loadDreamBenchRanking(root, file string) (dreamBenchState, error) {
	state, err := readDreamBenchState(file)
	if err != nil {
		return state, err
	}
	if age := time.Since(state.RankedAt); age >= 0 && age < 24*time.Hour {
		return state, nil
	}
	state.Ranking, err = rankDreamBenchPackages(root)
	if err != nil {
		return state, err
	}
	state.RankedAt = time.Now()
	return state, saveDreamBenchState(file, state)
}

func readDreamBenchState(file string) (dreamBenchState, error) {
	var state dreamBenchState
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("rotation state: %w", err)
	}
	for _, pkg := range state.Ranking {
		if !filepath.IsLocal(pkg) || strings.Contains(pkg, `\`) {
			return state, fmt.Errorf("rotation package is not local: %q", pkg)
		}
	}
	return state, nil
}

func nextDreamBenchPackages(ranking []string, last string) []string {
	if len(ranking) == 0 {
		return nil
	}
	start := (slices.Index(ranking, last) + 1) % len(ranking)
	selected := make([]string, 0, min(dreamRotationSize, len(ranking)))
	for i := range cap(selected) {
		selected = append(selected, ranking[(start+i)%len(ranking)])
	}
	return selected
}

func rankDreamBenchPackages(root string) ([]string, error) {
	pkgs, err := listDreamBenchPackages(root)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, pkg := range pkgs {
		for _, imported := range pkg.Imports {
			counts[imported]++
		}
	}
	slices.SortFunc(pkgs, func(a, b dreamGoPackage) int {
		if n := counts[b.ImportPath] - counts[a.ImportPath]; n != 0 {
			return n
		}
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
	var ranking []string
	for _, pkg := range pkgs {
		has, err := dreamPackageHasBench(pkg)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		rel, err := filepath.Rel(root, pkg.Dir)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("go list directory outside harness: %s", pkg.Dir)
		}
		ranking = append(ranking, filepath.ToSlash(rel))
	}
	return ranking, nil
}

func listDreamBenchPackages(root string) ([]dreamGoPackage, error) {
	data, err := dreamBenchCommand(root, time.Minute, "go", "list", "-json", "./...")
	if err != nil {
		return nil, fmt.Errorf("importer ranking: %w", err)
	}
	var pkgs []dreamGoPackage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg dreamGoPackage
		err := dec.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("go list JSON: %w", err)
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func dreamPackageHasBench(pkg dreamGoPackage) (bool, error) {
	files := append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...)
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && dreamBenchmarkName(fn.Name.Name) {
				return true, nil
			}
		}
	}
	return false, nil
}

func dreamBenchmarkName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Benchmark")
	if !ok {
		return false
	}
	for _, r := range rest {
		return !unicode.IsLower(r)
	}
	return true
}

func dreamBenchIdle() error {
	var data []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		data, err = os.ReadFile("/proc/loadavg")
	case "darwin":
		data, err = dreamBenchCommand("", 5*time.Second, "sysctl", "-n", "vm.loadavg")
	default:
		return fmt.Errorf("load average unavailable on %s", runtime.GOOS)
	}
	if err != nil {
		return fmt.Errorf("load average unavailable: %w", err)
	}
	fields := strings.Fields(strings.Trim(string(data), "{} \n\t"))
	if len(fields) == 0 {
		return errors.New("load average is empty")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || !(load >= 0 && load < float64(runtime.NumCPU())/2) {
		return fmt.Errorf("load %s is not below %.1f", fields[0], float64(runtime.NumCPU())/2)
	}
	return nil
}

func dreamBenchCommand(root string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.WaitDelay = root, time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

func runDreamBench(root, pkg string, out io.Writer) error {
	data, err := dreamBenchCommand(root, 35*time.Minute, "claude-code", "review", "bench", "--path", filepath.Join(root, pkg), "--count", "5", "--format", "json")
	if err != nil {
		return err
	}
	var report bench.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return fmt.Errorf("review bench JSON: %w", err)
	}
	queued := 0
	for _, result := range report.Packages {
		for _, score := range result.Benchmarks {
			if !dreamBenchMoved(score) {
				continue
			}
			entry := dreamBenchEntry{At: time.Now(), Source: "dream-rotation", Module: score.Name, Pkg: result.Pkg, Deltas: score.Deltas}
			if d, ok := score.Deltas["sec/op"]; ok {
				before, e1 := strconv.ParseFloat(d.Old, 64)
				after, e2 := strconv.ParseFloat(d.New, 64)
				if err := errors.Join(e1, e2); err != nil {
					return fmt.Errorf("benchmark duration: %w", err)
				}
				const msPerSecond = float64(time.Second / time.Millisecond)
				entry.BeforeMS, entry.AfterMS = before*msPerSecond, after*msPerSecond
			}
			if err := queueDreamBench(entry); err != nil {
				return err
			}
			queued++
		}
	}
	fmt.Fprintf(out, "memory dream: rotation: %s (%d significant benchmark(s) queued)\n", pkg, queued)
	return nil
}

func dreamBenchMoved(score bench.Score) bool {
	for _, delta := range score.Deltas {
		if delta.Significant {
			return true
		}
	}
	return false
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
