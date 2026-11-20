package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// auditCommand is the full audit started on main after a promote; empty skips it (tests).
var auditCommand = "make audit"

// auditStale is how long a start record without an exit file still counts as running.
const auditStale = time.Hour

// auditHungAfter is how long a running audit's log may sit unchanged before it is reported hung.
var auditHungAfter = 20 * time.Minute

// auditRecord is the start record <short-sha>.json; the exit file <short-sha>.exit ("<code> <seconds>") ends it.
type auditRecord struct {
	SHA     string `json:"sha"`
	Log     string `json:"log"`
	Started int64  `json:"started"`
}

func auditDir() string { return filepath.Join(paths.StateDir(), "worktree", "audit") }

func auditBase(sha string) string { return filepath.Join(auditDir(), ShortOID(sha)) }

// auditScript runs $2 in a clean detached worktree of commit $4 of repo $1 and records "<exit code> <seconds>" in $3.
const auditScript = `s=$(date +%s); t=$(mktemp -d) || exit 1; rc=1; ` +
	`if git -C "$1" worktree add --detach "$t/src" "$4" >&2; then (cd "$t/src" && bash -c "$2"); rc=$?; ` +
	`git -C "$1" worktree remove --force "$t/src" >&2; fi; rm -rf "$t"; ` +
	`echo "$rc $(( $(date +%s) - s ))" >"$3.tmp" && mv "$3.tmp" "$3"`

// StartAudit starts the detached audit of a clean snapshot of sha (taken at main); returns the line to print.
func StartAudit(main, sha string) string {
	if auditCommand == "" {
		return ""
	}
	base := auditBase(sha)
	if rec, err := readAuditRecord(base); err == nil && !paths.FileExists(base+".exit") &&
		time.Since(time.Unix(rec.Started, 0)) < auditStale {
		return "audit: already running on " + ShortOID(sha) + " (log " + rec.Log + ")"
	}
	if err := os.MkdirAll(auditDir(), 0o700); err != nil {
		return "audit: not started: " + err.Error()
	}
	_ = os.Remove(base + ".exit")
	_ = os.Remove(base + ".reported")
	_ = os.Remove(base + ".hung")
	logFile, err := os.Create(base + ".log")
	if err != nil {
		return "audit: not started: " + err.Error()
	}
	defer logFile.Close()
	data, err := json.Marshal(auditRecord{SHA: sha, Log: base + ".log", Started: time.Now().Unix()})
	if err == nil {
		err = paths.AtomicWrite(base+".json", data, 0o600)
	}
	if err != nil {
		return "audit: not started: " + err.Error()
	}
	if !proc.SpawnDetached("bash", []string{"-c", auditScript, "audit", main, auditCommand, base + ".exit", sha}, main, logFile) {
		return "audit: not started: cannot spawn bash"
	}
	return "audit: started on " + ShortOID(sha) + " (log " + base + ".log)"
}

func readAuditRecord(base string) (auditRecord, error) {
	var rec auditRecord
	data, err := os.ReadFile(base + ".json")
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	return rec, err
}

// AuditReports returns one line per finished audit not yet reported, marking each reported.
func AuditReports() []string {
	starts, _ := filepath.Glob(filepath.Join(auditDir(), "*.json"))
	var lines []string
	for _, f := range starts {
		base := strings.TrimSuffix(f, ".json")
		rec, err := readAuditRecord(base)
		if err != nil {
			continue
		}
		code, secs, ok := readAuditExit(base + ".exit")
		if !ok && auditHung(rec) && claimAuditReport(base+".hung") {
			lines = append(lines, fmt.Sprintf("audit of %s hung (no log output for %s): %s", ShortOID(rec.SHA), auditHungAfter, rec.Log))
			continue
		}
		if !ok || !claimAuditReport(base+".reported") {
			continue
		}
		if code == 0 {
			lines = append(lines, fmt.Sprintf("audit of %s passed (%ds)", ShortOID(rec.SHA), secs))
		} else {
			lines = append(lines, fmt.Sprintf("audit of %s FAILED (exit %d): %s — fix on main now", ShortOID(rec.SHA), code, rec.Log))
		}
	}
	return lines
}

// auditHung is true for a started audit (caller checked no exit file) whose log has not changed within auditHungAfter.
func auditHung(rec auditRecord) bool {
	fi, err := os.Stat(rec.Log)
	return err == nil && time.Since(fi.ModTime()) > auditHungAfter
}

// readAuditExit parses an exit file "<code> <seconds>"; ok is false while the audit has not ended.
func readAuditExit(file string) (code, secs int, ok bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, 0, false
	}
	f := strings.Fields(string(data))
	if len(f) != 2 {
		return 0, 0, false
	}
	code, err1 := strconv.Atoi(f[0])
	secs, err2 := strconv.Atoi(f[1])
	return code, secs, err1 == nil && err2 == nil
}

// claimAuditReport creates the reported marker exclusively; only the first caller across sessions gets true.
func claimAuditReport(marker string) bool {
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	return f.Close() == nil
}
