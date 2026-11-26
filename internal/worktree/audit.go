package worktree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// Hooks never link SQLite: a land appends to the spool; the auditq worker owns the table and publishes verdicts.json.

// auditCommand is the full audit started on main after a promote; empty skips it (tests).
var auditCommand = "make audit"

// SwapAuditCommand sets the post-promote audit command ("" skips it) and returns a restore func, for other packages' tests.
func SwapAuditCommand(cmd string) (restore func()) {
	old := auditCommand
	auditCommand = cmd
	return func() { auditCommand = old }
}

// AuditCommand is the command the worker runs in each audit slot.
func AuditCommand() string { return auditCommand }

// AuditStale is how long a queued audit with no run counts as in flight before it is reported hung.
const AuditStale = time.Hour

// AuditKeep is how long the worker keeps finished rows, markers and logs; reports skip rows within a day of it.
const AuditKeep = 7 * 24 * time.Hour

// auditHungAfter is how long a running audit's log may sit unchanged before it is reported hung.
var auditHungAfter = 20 * time.Minute

// AuditArg is the hidden verb (internal/cli/table.go) that runs the queue worker.
const AuditArg = "--internal-audit"

// auditSpawn starts the detached worker; .hookd hands the verb to claude-code beside it.
var auditSpawn = func() bool {
	self, err := paths.SelfBinary()
	return err == nil && proc.SpawnDetached(self, []string{AuditArg}, paths.StateDir(), nil)
}

// SwapAuditSpawn replaces the worker spawn and returns a restore func, for other packages' tests.
func SwapAuditSpawn(fn func() bool) (restore func()) {
	old := auditSpawn
	auditSpawn = fn
	return func() { auditSpawn = old }
}

// Audit row states.
const (
	AuditQueued  = "queued"
	AuditRunning = "running"
	AuditDone    = "done"
)

// AuditItem is one enqueued land; (SHA, Queued unix ns) is its token and the table's key, so a replay is a no-op.
type AuditItem struct {
	Repo   string `json:"repo"`
	Base   string `json:"base"`
	SHA    string `json:"sha"`
	Queued int64  `json:"queued"`
}

// AuditRow is one enqueue's table row as the worker publishes it; times are unix seconds.
type AuditRow struct {
	AuditItem
	State   string   `json:"state"`
	Started int64    `json:"started,omitempty"`
	Ended   int64    `json:"ended,omitempty"`
	Log     string   `json:"log,omitempty"`
	With    []string `json:"with,omitempty"`
	Code    int      `json:"code"`
	Secs    int      `json:"secs"`
}

// auditTarget matches an `audit:` rule line, not an `audit :=` variable assignment.
var auditTarget = regexp.MustCompile(`(?m)^audit\s*:($|[^=])`)

// hasAuditTarget reports whether the repo's Makefile defines an audit target.
func hasAuditTarget(repo string) bool {
	data, err := os.ReadFile(filepath.Join(repo, "Makefile"))
	return err == nil && auditTarget.Match(data)
}

// AuditDir holds the store, the spool, the published rows, report markers and run logs.
func AuditDir() string { return filepath.Dir(paths.AuditDBPath()) }

// AuditSpool is the append-only enqueue file; appends and the worker's import both hold its lock.
func AuditSpool() string { return filepath.Join(AuditDir(), "spool.jsonl") }

func auditSnapshot() string { return filepath.Join(AuditDir(), "verdicts.json") }

// startAudit queues the audit of sha (landed on main) for the one worker; returns the line to print.
func startAudit(main, sha string) string {
	if auditCommand == "" {
		return ""
	}
	if !hasAuditTarget(main) {
		return "audit: skipped, no audit target in " + main
	}
	mb, err := proc.RunGit(context.Background(), main, "merge-base", "HEAD", sha)
	if err != nil {
		return "audit: not started: merge-base: " + err.Error()
	}
	item := AuditItem{Repo: main, Base: strings.TrimSpace(mb), SHA: sha, Queued: time.Now().UnixNano()}
	running, err := queueAudit(item)
	switch {
	case err != nil:
		return "audit: not started: " + err.Error()
	case running:
		ensureWorker() // its worker may have died with the row in flight
		return "audit: already running on " + ShortOID(sha) + " (logs in " + AuditDir() + ")"
	case !auditSpawn(): // the line stays spooled; the next land's worker drains it
		return "audit: queued on " + ShortOID(sha) + ", worker not started (logs in " + AuditDir() + ")"
	}
	return "audit: started on " + ShortOID(sha) + " (logs in " + AuditDir() + ")"
}

// queueAudit appends item to the spool unless its sha is already in flight; the worker's import re-checks.
func queueAudit(item AuditItem) (running bool, err error) {
	line, err := json.Marshal(item)
	if err != nil {
		return false, err
	}
	spool := AuditSpool()
	err = paths.WithFileLock(spool, func() error {
		rows, err := auditRows()
		if err != nil {
			return err
		}
		if running = auditInFlight(item.SHA, rows); running {
			return nil
		}
		if err = paths.AppendLineRepair(spool, line); err != nil {
			return err
		}
		return syncFile(spool) // "started" promises the line survives a host crash
	})
	return running, err
}

// syncFile fsyncs path and, best effort (Windows cannot), its directory, which may have just gained it.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// auditRows is the published rows plus spool lines whose token the worker has not imported; hold the spool lock.
func auditRows() ([]AuditRow, error) {
	items, err := ReadAuditSpool(AuditSpool())
	if err != nil {
		return nil, err
	}
	rows := readAuditSnapshot()
	imported := map[AuditItem]bool{}
	for _, r := range rows {
		imported[r.AuditItem] = true
	}
	for _, r := range spooled(items) {
		if !imported[r.AuditItem] {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// auditInFlight reports whether sha has a queued or running row touched within AuditStale.
func auditInFlight(sha string, rows []AuditRow) bool {
	for _, r := range rows {
		if r.SHA == sha && r.State != AuditDone && time.Since(rowTouched(r)) < AuditStale {
			return true
		}
	}
	return false
}

// rowTouched is the latest of the row's enqueue, run start and end.
func rowTouched(r AuditRow) time.Time {
	return time.Unix(max(r.Queued/int64(time.Second), r.Started, r.Ended), 0)
}

// nearPrune is true within a day of the worker's prune, which may delete the row and its marker under a reader.
func nearPrune(r AuditRow) bool { return time.Since(rowTouched(r)) > AuditKeep-24*time.Hour }

// ReadAuditSpool returns the spool's complete lines (a torn last line waits); only a missing spool is empty.
func ReadAuditSpool(file string) ([]AuditItem, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []AuditItem
	for l := range bytes.SplitAfterSeq(data, []byte("\n")) {
		if len(l) == 0 || l[len(l)-1] != '\n' {
			break
		}
		var it AuditItem
		if json.Unmarshal(l, &it) == nil && it.Repo != "" && it.SHA != "" && it.Queued > 0 {
			items = append(items, it)
		}
	}
	return items, nil
}

// spooled is items as queued rows, for the reports and in-flight checks before the worker imports them.
func spooled(items []AuditItem) []AuditRow {
	rows := make([]AuditRow, len(items))
	for i, it := range items {
		rows[i] = AuditRow{AuditItem: it, State: AuditQueued}
	}
	return rows
}

func readAuditSnapshot() []AuditRow {
	var rows []AuditRow
	if data, err := os.ReadFile(auditSnapshot()); err == nil {
		_ = json.Unmarshal(data, &rows)
	}
	return rows
}

// WriteAuditSnapshot publishes the table's rows; the worker calls it under the spool lock, so writes never reorder.
func WriteAuditSnapshot(rows []AuditRow) error {
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(auditSnapshot(), data, 0o600)
}

// AuditVerdict is one sha's audit state: hung, or finished with its exit code and seconds.
type AuditVerdict struct {
	Row        AuditRow
	Hung       bool
	Code, Secs int
}

// auditVerdict decides passed / FAILED / hung for one row; ok is false while it is still running normally.
func auditVerdict(r AuditRow) (v AuditVerdict, ok bool) {
	v.Row = r
	if r.State != AuditDone {
		v.Hung = auditHung(r)
		return v, v.Hung
	}
	v.Code, v.Secs = r.Code, r.Secs
	return v, true
}

// AuditReports returns one line per finished or hung audit not yet reported, marking each reported.
func AuditReports() []string {
	if !paths.FileExists(AuditDir()) {
		return nil
	}
	var lines []string
	pending := false
	// under the spool lock a publication never lands between the reads and the claims; busy: the next prompt retries
	_ = paths.WithFileLockWithin(AuditSpool(), reportLockWait, func() error {
		rows, err := auditRows()
		if err == nil {
			lines = reportLines(rows)
			pending = slices.ContainsFunc(rows, func(r AuditRow) bool { return r.State != AuditDone })
		}
		return err
	})
	if pending { // work with no worker left (a crash, a spawn that failed): every prompt restarts it
		ensureWorker()
	}
	return lines
}

// AuditWorkerLock is the flock the one draining worker process holds; nothing it starts inherits it.
func AuditWorkerLock() string { return filepath.Join(AuditDir(), "worker.lock") }

// spawnCooldown spaces restarts, so a worker that keeps failing is not respawned by every prompt of every session.
var spawnCooldown = 15 * time.Second

// ensureWorker spawns the worker when none holds its flock and none was restarted within spawnCooldown.
func ensureWorker() {
	f, err := os.OpenFile(AuditWorkerLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	stamp := filepath.Join(AuditDir(), "worker.spawn")
	spawn := paths.TryLock(f) // held for an instant: the stamp check and touch are one step across sessions
	if fi, err := os.Stat(stamp); spawn && err == nil {
		age := time.Since(fi.ModTime())
		spawn = age < 0 || age >= spawnCooldown // a stamp from the future (clock step) never blocks
	}
	if spawn {
		spawn = os.WriteFile(stamp, nil, 0o600) == nil
	}
	_ = f.Close()
	if spawn {
		auditSpawn()
	}
}

// reportLockWait bounds how long a prompt hook waits for the worker's publication.
const reportLockWait = 250 * time.Millisecond

func reportLines(rows []AuditRow) []string {
	var lines []string
	for _, r := range rows {
		v, ok := auditVerdict(r)
		if !ok || nearPrune(r) || !claimAuditReport(v) {
			continue
		}
		var line string
		switch {
		case v.Hung:
			line = fmt.Sprintf("audit of %s hung in %s (no log output for %s): %s", ShortOID(r.SHA), r.Repo, auditHungAfter, r.Log)
		case v.Code == 0:
			line = fmt.Sprintf("audit of %s passed (%ds)", ShortOID(r.SHA), v.Secs)
		default:
			line = fmt.Sprintf("audit of %s FAILED (exit %d) in %s: %s — fix on main now", ShortOID(r.SHA), v.Code, r.Repo, r.Log)
		}
		lines = append(lines, line+runWith(r.With))
	}
	return lines
}

// AuditMarker is one enqueue's report-marker stem: a token's result never changes, so a claim needs no undo.
func AuditMarker(it AuditItem) string {
	return filepath.Join(AuditDir(), ShortOID(it.SHA)+"-"+strconv.FormatInt(it.Queued, 10))
}

// claimAuditReport creates v's marker exclusively; only the first caller across sessions gets true.
func claimAuditReport(v AuditVerdict) bool {
	ext := ".reported"
	if v.Hung {
		ext = ".hung"
	}
	f, err := os.OpenFile(AuditMarker(v.Row.AuditItem)+ext, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

// runWith is " (run with <short>, ...)" for an audit that covered other shas, else "".
func runWith(with []string) string {
	if len(with) == 0 {
		return ""
	}
	short := make([]string, len(with))
	for i, w := range with {
		short[i] = ShortOID(w)
	}
	return " (run with " + strings.Join(short, ", ") + ")"
}

// auditHung: a run whose log is idle past auditHungAfter, or no log and untouched for longer than AuditStale.
func auditHung(r AuditRow) bool {
	if fi, err := os.Stat(r.Log); r.Log != "" && err == nil {
		return time.Since(fi.ModTime()) > auditHungAfter
	}
	return r.Queued > 0 && time.Since(rowTouched(r)) > AuditStale
}
