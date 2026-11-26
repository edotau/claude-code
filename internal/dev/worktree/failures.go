package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/health"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
)

// init wires dream's hook_p95 report here, since memory cannot import worktree; every binary linking both gets it.
func init() {
	memory.ReportFailure = func(kind, subject, detail string) error {
		_, err := ReportFailure(FailuresDir(), Failure{Kind: FailureKind(kind), Subject: subject, Summary: detail,
			Log: filepath.Join(paths.StateDir(), "hooks"), Time: time.Now().Unix()})
		return err
	}
	agentconfig.ReportUnknownKey = func(keys []string, origin string) {
		names := slices.Compact(slices.Sorted(slices.Values(keys)))
		more := ""
		if len(names) > 10 {
			more, names = fmt.Sprintf(" +%d more", len(names)-10), names[:10]
		}
		subject := origin + " " + strings.Join(names, ",") + more
		_, _ = reportFailure(FailuresDir(), Failure{Kind: FailOverlayKey, Subject: subject,
			Summary: "unknown overlay keys (" + origin + "): " + strings.Join(names, ",") + more, Time: time.Now().Unix()},
			func(p string, fn func() error) error { return paths.WithFileLockWithin(p, overlayReportWait, fn) })
	}
	health.Report = func(reason, subject string) {
		_, _ = ReportFailure(FailuresDir(), Failure{Kind: FailDegraded, Subject: reason + " " + subject,
			Summary: health.Notice(reason), Log: filepath.Join(paths.StateDir(), "agent", "health.json"), Time: time.Now().Unix()})
	}
}

// FailureKind is the typed set of failures that broadcast to live sessions.
type FailureKind string

const (
	FailAudit      FailureKind = "audit"
	FailLaneGate   FailureKind = "lane_gate"
	FailPlanCheck  FailureKind = "plan_verify"
	FailBench      FailureKind = "bench"
	FailDispatch   FailureKind = "dispatch"
	FailContract   FailureKind = "contract"
	FailProxyDown  FailureKind = "proxy_down"
	FailProxy5xx   FailureKind = "proxy_5xx"
	FailHookP95    FailureKind = "hook_p95"
	FailDegraded   FailureKind = "dispatch_degraded"
	FailOverlayKey FailureKind = "overlay_unknown_key"
)

// FailureKinds lists every valid kind, in display order.
var FailureKinds = []FailureKind{FailAudit, FailLaneGate, FailPlanCheck, FailBench, FailDispatch, FailContract,
	FailProxyDown, FailProxy5xx, FailHookP95, FailDegraded, FailOverlayKey}

// DirectiveKinds reach a main session as a fix-now directive; a kind joins this closed list once it is logged.
var DirectiveKinds = []FailureKind{FailAudit, FailLaneGate, FailProxyDown, FailProxy5xx, FailContract}

// Failure is one broadcast record; Time is unix seconds; Seq grows by one per appended record.
type Failure struct {
	ID      string      `json:"id"`
	Kind    FailureKind `json:"kind"`
	Subject string      `json:"subject"`
	Summary string      `json:"summary"`
	Log     string      `json:"log,omitempty"`
	Time    int64       `json:"time"`
	Seq     int64       `json:"seq"`
	Repo    string      `json:"repo,omitempty"`
}

const (
	failureWindow  = 30 * time.Minute // same ID inside it is dropped (D1)
	failureFresh   = 24 * time.Hour   // a session with no cursor skips older records
	failureSummary = 160
	failureMaxSize = 1 << 20
	failureKeep    = 500
)

// FailuresDir is the default root; a subdirectory so the per-session inbox glob never matches the log.
func FailuresDir() string { return filepath.Join(noticesDir(), "failures") }

// FailureID is the stable id of a kind and subject.
func FailureID(kind FailureKind, subject string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + subject))
	return hex.EncodeToString(sum[:])[:12]
}

func (k FailureKind) valid() bool { return slices.Contains(FailureKinds, k) }

func failuresLog(root string) string { return filepath.Join(root, "failures.jsonl") }

// failureIDPath rejects ids that are not a FailureID, so a caller cannot reach outside root.
func failureIDPath(root, id, suffix string) (string, error) {
	if _, err := hex.DecodeString(id); err != nil || len(id) != 12 {
		return "", fmt.Errorf("failures: bad id %q", id)
	}
	return claimPath(root, id) + suffix, nil
}

// oneLine collapses s to one line of at most n bytes, cut on a rune boundary.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}

// ReportFailure appends f unless the same ID was logged inside the dedupe window; it reports whether it appended.
func ReportFailure(root string, f Failure) (bool, error) {
	return reportFailure(root, f, paths.WithFileLock)
}

// overlayReportWait caps how long an overlay load waits for the failure log lock; a timeout is swallowed.
const overlayReportWait = 300 * time.Millisecond

func reportFailure(root string, f Failure, lock func(string, func() error) error) (bool, error) {
	if !f.Kind.valid() {
		return false, fmt.Errorf("failures: unknown kind %q", f.Kind)
	}
	f.Summary, f.Subject, f.Log = oneLine(f.Summary, failureSummary), oneLine(f.Subject, 200), oneLine(f.Log, 300)
	if f.Subject == "" {
		return false, errors.New("failures: no subject")
	}
	f.ID = FailureID(f.Kind, f.Subject)
	if now := time.Now().Unix(); f.Time <= 0 || f.Time > now {
		f.Time = now
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return false, err
	}
	appended := false
	err := lock(failuresLog(root), func() (err error) {
		appended, err = appendFailure(root, f)
		return err
	})
	return appended, err
}

// appendFailure runs under the log lock: dedupe unless fixed, number, append, reopen, trim.
func appendFailure(root string, f Failure) (bool, error) {
	file := failuresLog(root)
	seq := readInt(filepath.Join(root, "seq"))
	recs, err := ListFailures(root)
	if err != nil {
		return false, err
	}
	if _, fixed := FailureHolder(root, f.ID); !fixed && duplicateFailure(recs, f) {
		return false, nil
	}
	for _, r := range recs {
		seq = max(seq, r.Seq) // a lost counter file never reuses a logged Seq
	}
	f.Seq = seq + 1
	line, err := json.Marshal(f)
	if err != nil {
		return false, err
	}
	if err := paths.AtomicWriteNoSync(filepath.Join(root, "seq"), strconv.AppendInt(nil, f.Seq, 10), 0o600); err != nil {
		return false, err
	}
	if err := appendRaw(file, line); err != nil {
		return false, err
	}
	reopenFailure(root, f.ID)
	return true, trimFailures(file)
}

// duplicateFailure reports whether recs already hold f's ID inside the dedupe window.
func duplicateFailure(recs []Failure, f Failure) bool {
	return slices.ContainsFunc(recs, func(r Failure) bool {
		return r.ID == f.ID && f.Time-r.Time < int64(failureWindow/time.Second)
	})
}

// appendRaw adds one line to file, first ending a torn last line; there is no fsync, a lost append only loses a notice.
func appendRaw(file string, line []byte) error {
	if endsTorn(file) {
		line = append([]byte{'\n'}, line...)
	}
	w, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := w.Write(append(line, '\n'))
	if cerr := w.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// reopenFailure drops a finished failure's claim so its recurrence can be claimed again.
func reopenFailure(root, id string) {
	if _, err := os.Stat(claimPath(root, id) + ".done"); err == nil {
		_ = os.Remove(claimPath(root, id) + ".done")
		_ = os.Remove(claimPath(root, id))
		_ = os.Remove(claimPath(root, id) + ".offer")
	}
}

// endsTorn reports whether the log's last byte is not a newline (a crashed partial append).
func endsTorn(file string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false
	}
	var b [1]byte
	_, err = f.ReadAt(b[:], st.Size()-1)
	return err == nil && b[0] != '\n'
}

func claimPath(root, id string) string { return filepath.Join(root, "claims", id) }

// trimFailures keeps the newest failureKeep records once the log passes failureMaxSize.
func trimFailures(file string) error {
	st, err := os.Stat(file)
	if err != nil || st.Size() <= failureMaxSize {
		return err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := bytes.SplitAfter(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) <= failureKeep {
		return nil
	}
	return paths.AtomicWrite(file, append(bytes.Join(lines[len(lines)-failureKeep:], nil), '\n'), 0o600)
}

// readInt reads a decimal counter file; a missing or bad file is 0.
func readInt(file string) int64 {
	raw, _ := os.ReadFile(file)
	n, _ := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

// ListFailures returns every logged record, oldest first; a missing log is empty, an unreadable one an error.
func ListFailures(root string) ([]Failure, error) {
	raw, err := os.ReadFile(failuresLog(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Failure
	for l := range bytes.SplitSeq(raw, []byte("\n")) {
		var r Failure
		if json.Unmarshal(l, &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// FailuresSinceWithin returns up to limit unseen records and the skipped count; a lock timeout leaves the cursor.
func FailuresSinceWithin(root, slug string, limit int, wait time.Duration) (lines []Failure, more int, err error) {
	cursor := filepath.Join(root, "cursors", "s-"+SessionSlug(slug))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, 0, err
	}
	err = paths.WithFileLockWithin(failuresLog(root), wait, func() error {
		seen, fresh := readInt(cursor), time.Now().Add(-failureFresh).Unix()
		_, known := os.Stat(cursor)
		last := seen
		recs, lerr := ListFailures(root)
		if lerr != nil {
			return lerr
		}
		for _, r := range recs {
			last = max(last, r.Seq)
			switch {
			case r.Seq <= seen || known != nil && r.Time < fresh:
			case len(lines) < limit:
				lines = append(lines, r)
			default:
				more++
			}
		}
		if known == nil && last == seen { // nothing new: the cursor already holds last
			return nil
		}
		return paths.AtomicWriteNoSync(cursor, strconv.AppendInt(nil, last, 10), 0o600)
	})
	return lines, more, err
}

// ClaimFailure returns id's holder: slug when it won, else the first claimant ("autofix" for an autofix branch).
func ClaimFailure(root, id, slug string) (holder string, err error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	err = paths.WithFileLock(failuresLog(root), func() error {
		holder, err = claimFailure(root, id, slug)
		return err
	})
	return holder, err
}

func claimFailure(root, id, slug string) (string, error) {
	claim, err := failureIDPath(root, id, "")
	if err != nil {
		return "", err
	}
	slug = SessionSlug(slug)
	if raw, rerr := os.ReadFile(claim); rerr == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	if autofixHeld(root, id) {
		return "autofix", nil
	}
	if err := os.MkdirAll(filepath.Dir(claim), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(claim), ".claim-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.WriteString(slug)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	// a hard link publishes the claim whole and fails when one exists
	if err := os.Link(tmp.Name(), claim); os.IsExist(err) {
		raw, rerr := os.ReadFile(claim)
		return strings.TrimSpace(string(raw)), rerr
	} else if err != nil {
		return "", err
	}
	return slug, nil
}

// FailureHolder returns who holds id ("" when open) and whether it is done.
func FailureHolder(root, id string) (holder string, done bool) {
	claim, err := failureIDPath(root, id, "")
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(claim + ".done"); err == nil {
		done = true
	}
	raw, _ := os.ReadFile(claim)
	return strings.TrimSpace(string(raw)), done
}

// DoneFailure marks id fixed.
func DoneFailure(root, id string) error {
	claim, err := failureIDPath(root, id, ".done")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(claim), 0o700); err != nil {
		return err
	}
	return os.WriteFile(claim, nil, 0o600)
}

// autofixHeld reports whether the record id names a "<lane>@<sha7>" subject whose autofix branch exists in its repo.
func autofixHeld(root, id string) bool {
	recs, _ := ListFailures(root)
	return slices.ContainsFunc(recs, func(r Failure) bool { return r.ID == id && autofixBranch(context.Background(), r) })
}

// autofixBranch reports whether r's "<lane>@<sha7>" subject has an autofix branch in r's repo, probed on ctx.
func autofixBranch(ctx context.Context, r Failure) bool {
	_, sha, ok := strings.Cut(r.Subject, "@")
	if r.Repo == "" || !ok || len(sha) != 7 {
		return false
	}
	ref := "refs/heads/worktree-autofix-" + sha
	if found, ok := refInGitDir(filepath.Join(r.Repo, ".git"), ref); ok {
		return found
	}
	cmd := exec.CommandContext(ctx, "git", "-C", r.Repo, "show-ref", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

// refInGitDir reads ref from a plain .git dir without a spawn; ok=false (ask git) for a .git file, reftable or error.
func refInGitDir(gitDir, ref string) (found, ok bool) {
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "reftable")); err == nil {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "refs", "heads", strings.TrimPrefix(ref, "refs/heads/"))); err == nil {
		return true, true
	}
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if os.IsNotExist(err) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	for l := range bytes.SplitSeq(packed, []byte("\n")) {
		if _, name, cut := strings.Cut(string(l), " "); cut && name == ref {
			return true, true
		}
	}
	return false, true
}

// OfferResult is what one session gets of an open failure: nothing, the fix-now directive, or the plain fallback.
type OfferResult int

const (
	OfferNone OfferResult = iota
	OfferDirective
	OfferNotice // DirectiveMax deliveries went unclaimed: the plain notice, once
	OfferOwned  // a claim, done mark or autofix branch holds it: nothing, and its plain line stays
)

const (
	DirectiveMax  = 2                      // directive deliveries with no claim before the plain notice and doctor row
	offerLease    = 10 * time.Minute       // a delivered directive is the session's alone this long before another gets it
	offerLockWait = 200 * time.Millisecond // hooks call it: a busy log is skipped, never waited on
)

// failureOffer is an id's directive deliveries: the sessions, the last delivery time, and whether the fallback went.
type failureOffer struct {
	Sessions []string `json:"sessions"`
	Time     int64    `json:"time"`
	Noticed  bool     `json:"noticed,omitempty"`
}

func readOffer(file string) (o failureOffer) {
	raw, _ := os.ReadFile(file)
	_ = json.Unmarshal(raw, &o) // a missing or torn record is no delivery yet
	return o
}

// OfferFailure decides under the log lock what slug gets of f now and records it; fallback allows OfferNotice.
func OfferFailure(ctx context.Context, root string, f Failure, slug string, fallback bool) (OfferResult, error) {
	file, err := failureIDPath(root, f.ID, ".offer")
	if err != nil {
		return OfferNone, err
	}
	slug = SessionSlug(slug)
	if _, res := nextOffer(readOffer(file), slug, time.Now().Unix(), fallback); res == OfferNone {
		return OfferNone, nil // nothing due: no lock
	}
	if !FailureOpen(ctx, root, f) { // the git probe runs on ctx, outside the lock
		return OfferOwned, ctx.Err()
	}
	res := OfferNone
	err = paths.WithFileLockWithin(failuresLog(root), offerLockWait, func() error {
		if holder, done := FailureHolder(root, f.ID); holder != "" || done || ctx.Err() != nil {
			return nil // a spent hook budget records nothing it could not return
		}
		var o failureOffer
		if o, res = nextOffer(readOffer(file), slug, time.Now().Unix(), fallback); res == OfferNone {
			return nil
		}
		return writeOffer(file, o)
	})
	if err != nil {
		return OfferNone, err
	}
	return res, nil
}

func writeOffer(file string, o failureOffer) error {
	raw, err := json.Marshal(o)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(file), 0o700)
	}
	if err == nil {
		err = paths.AtomicWriteNoSync(file, raw, 0o600)
	}
	return err
}

// nextOffer is o after slug asks at now: one session per lease, DirectiveMax deliveries, then the fallback once.
func nextOffer(o failureOffer, slug string, now int64, fallback bool) (failureOffer, OfferResult) {
	switch {
	case len(o.Sessions) > 0 && now-o.Time < int64(offerLease/time.Second):
		return o, OfferNone
	case len(o.Sessions) >= DirectiveMax:
		if o.Noticed || !fallback {
			return o, OfferNone
		}
		o.Noticed, o.Time = true, now
		return o, OfferNotice
	}
	o.Sessions, o.Time = append(slices.Clone(o.Sessions), slug), now
	return o, OfferDirective
}

// UnclaimedDirectives is the open records whose directive went out DirectiveMax times with no claim, newest first.
func UnclaimedDirectives(ctx context.Context, root string) ([]Failure, error) {
	recs, err := ListFailures(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Failure
	for _, r := range slices.Backward(recs) {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if len(readOffer(claimPath(root, r.ID)+".offer").Sessions) >= DirectiveMax && FailureOpen(ctx, root, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// FailureOpen reports whether nobody owns f: no claim, not done, and no autofix branch (probed on ctx) for it.
func FailureOpen(ctx context.Context, root string, f Failure) bool {
	holder, done := FailureHolder(root, f.ID)
	return holder == "" && !done && !autofixBranch(ctx, f)
}

const promptKeep = 7 * 24 * time.Hour // a session stamp older than this is pruned when a new session stamps

func promptStamp(root, slug string) string {
	return filepath.Join(root, "prompts", "s-"+SessionSlug(slug))
}

// NotePrompt records now as slug's last user prompt; a session's first stamp also prunes week-old ones.
func NotePrompt(root, slug string) error {
	dir, stamp := filepath.Join(root, "prompts"), promptStamp(root, slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, statErr := os.Stat(stamp)
	if err := paths.AtomicWriteNoSync(stamp, strconv.AppendInt(nil, time.Now().Unix(), 10), 0o600); err != nil { // never behind the prune lock
		return err
	}
	if !os.IsNotExist(statErr) {
		return nil
	}
	err := paths.WithFileLockWithin(dir, offerLockWait, func() error {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			info, err := e.Info()
			if err != nil || e.Name() == filepath.Base(stamp) || time.Since(info.ModTime()) <= promptKeep {
				continue
			}
			if again, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && time.Since(again.ModTime()) > promptKeep {
				_ = os.Remove(filepath.Join(dir, e.Name())) // best-effort; the age is read again so a refreshed stamp stays
			}
		}
		return nil
	})
	if errors.Is(err, paths.ErrLockTimeout) {
		return nil // the next first stamp prunes
	}
	return err
}

// LastPrompt is slug's last user prompt; zero when none was recorded, now when a stamp exists but cannot be read.
func LastPrompt(root, slug string) time.Time {
	raw, err := os.ReadFile(promptStamp(root, slug))
	switch n, perr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); {
	case os.IsNotExist(err):
		return time.Time{}
	case err != nil || perr != nil || n <= 0:
		return time.Now() // presence unknown reads as present: investigate and wait, never apply
	default:
		return time.Unix(n, 0)
	}
}
