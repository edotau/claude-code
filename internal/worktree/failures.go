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
	"github.com/edotau/claude-code/internal/paths"
)

// init wires dream's hook_p95 report here, since memory cannot import worktree; every binary linking both gets it.
func init() {
	memory.ReportFailure = func(kind, subject, detail string) error {
		_, err := ReportFailure(FailuresDir(), Failure{Kind: FailureKind(kind), Subject: subject, Summary: detail,
			Log: filepath.Join(paths.StateDir(), "hooks"), Time: time.Now().Unix()})
		return err
	}
	health.Report = func(reason, subject string) {
		_, _ = ReportFailure(FailuresDir(), Failure{Kind: FailDegraded, Subject: reason + " " + subject,
			Summary: health.Notice(reason), Log: filepath.Join(paths.StateDir(), "agent", "health.json"), Time: time.Now().Unix()})
	}
}

// FailureKind is the typed set of failures that broadcast to live sessions.
type FailureKind string

const (
	FailAudit     FailureKind = "audit"
	FailLaneGate  FailureKind = "lane_gate"
	FailPlanCheck FailureKind = "plan_verify"
	FailBench     FailureKind = "bench"
	FailDispatch  FailureKind = "dispatch"
	FailContract  FailureKind = "contract"
	FailProxyDown FailureKind = "proxy_down"
	FailProxy5xx  FailureKind = "proxy_5xx"
	FailHookP95   FailureKind = "hook_p95"
	FailDegraded  FailureKind = "dispatch_degraded"
)

// FailureKinds lists every valid kind, in display order.
var FailureKinds = []FailureKind{FailAudit, FailLaneGate, FailPlanCheck, FailBench, FailDispatch, FailContract,
	FailProxyDown, FailProxy5xx, FailHookP95, FailDegraded}

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
	err := paths.WithFileLock(failuresLog(root), func() (err error) {
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

// FailuresSince returns up to limit records slug has not seen and how many more it skipped, then advances its cursor.
func FailuresSince(root, slug string, limit int) (lines []Failure, more int, err error) {
	return failuresSince(root, slug, limit, paths.WithFileLock)
}

// FailuresSinceWithin is FailuresSince with a bounded lock wait; on paths.ErrLockTimeout the cursor has not moved.
func FailuresSinceWithin(root, slug string, limit int, wait time.Duration) ([]Failure, int, error) {
	return failuresSince(root, slug, limit, func(p string, fn func() error) error {
		return paths.WithFileLockWithin(p, wait, fn)
	})
}

func failuresSince(root, slug string, limit int, lock func(string, func() error) error) (lines []Failure, more int, err error) {
	cursor := filepath.Join(root, "cursors", "s-"+paths.Slug(slug, "", 80, "session"))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, 0, err
	}
	err = lock(failuresLog(root), func() error {
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
	slug = paths.Slug(slug, "", 80, "session")
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
	for _, r := range recs {
		if r.ID != id || r.Repo == "" {
			continue
		}
		_, sha, ok := strings.Cut(r.Subject, "@")
		if !ok || len(sha) != 7 {
			continue
		}
		cmd := exec.CommandContext(context.Background(), "git", "-C", r.Repo, "show-ref", "--verify", "--quiet", "refs/heads/worktree-autofix-"+sha)
		if cmd.Run() == nil {
			return true
		}
	}
	return false
}
