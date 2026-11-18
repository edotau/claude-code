package memory

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// idRef is a block reference: a persisted 8-hex id (or id-alias) or a z-prefixed contentRef.
var idRef = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(z[0-9a-f]{8}|[0-9a-f]{8}(-[a-z0-9-]+)?)$`)
})

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
	hits := SearchBank(o.Query, o.K)
	logHits(hits, AccessSearch, now)
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
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
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
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
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
	return handoffClaim{request: string(b), block: "\n\nAlso write a handoff to exactly " + fields[1] + " (this name, not another): " +
		"first line exactly " + handoffIdentity(sessionID) + "; then " +
		"## State, ## In flight, ## Next, from this transcript, so a compacted session resumes from it."}
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

// ResolveBank returns the session root.
func ResolveBank() string { return root("") }

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
	fmt.Fprintf(w, "bank:    %s\nhome:    %s\nsession-start injects it; `claude-code memory update` (or the background update) writes it.\n", dir, SubDir(""))
	if isProjectContextTemplate() {
		ref, err := Reference("init")
		if err != nil {
			return err
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, ref)
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
	root := ResolveBank()
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
	hasStore := scaffolded()
	var rotated int
	err := withFlatLock(BankLockWait, func() error {
		var serr error
		rotated, serr = settleBankLocked(time.Now())
		return serr
	})
	if err != nil {
		return fmt.Errorf("memory index: %w", err)
	}
	if hasStore {
		fmt.Fprintf(out, "indexed %s", SubDir(""))
		if rotated > 0 {
			fmt.Fprintf(out, " (rotated %d sessionHistory entries to archive/)", rotated)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintf(out, "no store at %s; run `claude-code memory init` in the repo to create one\n", SubDir(""))
	}
	refreshBankNotice(errw, "memory index", root)
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
			fmt.Fprintf(out, "embedded %d of %d entries (%s)\n", n, total, c.Model)
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
		session = os.Getenv(SessionIDEnv)
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
		ref, err := Reference("update")
		if err != nil {
			return err
		}
		fmt.Fprintln(o.Out, ref)
		return nil
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "memory update (dry run): session %s, transcript %s, bank %s\n", session, transcript, SubDir(""))
		return nil
	}
	return update(updateOptions{Run: o.Run, Transcript: transcript, CWD: root, Session: session, Historical: o.Historical, Out: o.Out})
}

// DreamRunOptions is one `memory dream`.
type DreamRunOptions struct {
	CWD           string
	Force, DryRun bool
	Run           AgentFunc
	Out           io.Writer
}

// RunDream previews (DryRun), skips when not due, or consolidates the bank.
func RunDream(o DreamRunOptions) error {
	root := o.CWD
	if root == "" {
		var err error
		if root, err = os.Getwd(); err != nil {
			return err
		}
	}
	if o.DryRun {
		fmt.Fprint(o.Out, previewDream().Report())
		return nil
	}
	if !o.Force && !dreamDue(time.Now()) {
		fmt.Fprintln(o.Out, "memory dream: not due")
		return nil
	}
	return dream(dreamOptions{CWD: root, Run: o.Run, Out: o.Out, IfDue: !o.Force})
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
	own, err := bankFmtTargets()
	if err != nil {
		return err
	}
	var changed []string
	err = withFlatLock(BankLockWait, func() (ferr error) {
		changed, ferr = formatFiles(own, false)
		if len(changed) > 0 { // commit what was written even when a later file failed
			if cerr := commitStore("fmt: wrap bank lines", StoreRepo().dropCredentialShaped(storeRels(changed), nil)...); cerr != nil {
				ferr = errors.Join(ferr, cerr)
			}
		}
		return ferr
	})
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

// FormatCheck writes nothing and fails when any bank file would wrap.
func FormatCheck(w io.Writer) (dirty bool, err error) {
	changed, err := formatBank(true)
	if err != nil {
		return false, err
	}
	for _, f := range changed {
		fmt.Fprintln(w, f)
	}
	if len(changed) > 0 {
		fmt.Fprintf(w, "%d file(s) would change (run `claude-code memory fmt`)\n", len(changed))
		return true, nil
	}
	fmt.Fprintln(w, "memory fmt --check: clean")
	return false, nil
}

// ReviewOptions is one `memory review`.
type ReviewOptions struct {
	StaleDays                 int
	Brief, Notice, Candidates bool
}

// ReviewReport writes doctor + stats (Notice refreshes the cached session-start notice and prints nothing).
func ReviewReport(w io.Writer, o ReviewOptions) error {
	root := ResolveBank()
	if o.Notice {
		return cacheDoctorNotice(doctorBank(root, o.StaleDays, time.Now()))
	}
	problems := doctorBank(root, o.StaleDays, time.Now())
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
	fmt.Fprint(w, collectBankStats().Report())
	if !o.Brief && !o.Candidates {
		writeHookSignals(w, time.Now())
	}
	if o.Candidates {
		ref, err := Reference("review")
		if err != nil {
			return err
		}
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
		fmt.Fprintf(w, "%s  update   %s\n", e.Time.Format("2006-01-02 15:04"), e.Line)
	}
	if len(sessions) > 0 {
		fmt.Fprintln(w, "-- sessionHistory --")
		for _, s := range sessions {
			fmt.Fprintln(w, "- "+s)
		}
	}
	return nil
}

//go:embed prompts/*.md
var promptsFS embed.FS

// Reference loads verb's embedded prompts/<verb>.md; a missing or empty file errs, never a silent empty prompt.
func Reference(verb string) (string, error) {
	data, err := promptsFS.ReadFile("prompts/" + verb + ".md")
	if err != nil {
		return "", fmt.Errorf("memory reference %s: %w", verb, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", fmt.Errorf("memory reference %s: prompts/%s.md is empty", verb, verb)
	}
	return text, nil
}
