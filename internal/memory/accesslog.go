package memory

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// AccessLogFile is the append-only recall trace beside a bank: one line per read, the signal
// `AccessNominations` reads. Bank-adjacent beacon — NOT a typed memory file; never injected.
const AccessLogFile = "access.log"

const (
	accessEntryMax   = 120
	accessReasonMax  = 60
	accessLogMaxByte = 256 << 10
	accessLogKeep    = 2000 // newest lines kept when the size cap trims
)

// LogAccess appends one `<RFC3339> -- <entry> -- <reason>` line to dir's access log. Fail-open:
// errors are swallowed. A credential-shaped reason is redacted whole (unscanned-sink risk).
func LogAccess(dir string, now time.Time, entry, reason string) {
	LogAccessBatch(dir, now, []string{entry}, reason)
}

// LogAccessBatch appends a recall batch with one open and write per bank.
func LogAccessBatch(dir string, now time.Time, entries []string, reason string) {
	if dir == "" || len(entries) == 0 {
		return
	}
	if credentialPattern.MatchString(reason) {
		reason = "(reason redacted — credential-shaped)"
	}
	var lines strings.Builder
	prefix := now.UTC().Format(time.RFC3339) + " -- "
	suffix := " -- " + sanitizeField(reason, accessReasonMax) + "\n"
	for _, entry := range entries {
		lines.WriteString(prefix + sanitizeField(entry, accessEntryMax) + suffix)
	}
	path := AccessLogPath(dir)
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return // symlink/oddity planted at the log name — never write through it
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return // log dir not scaffolded yet — fail-open, same as every other LogAccess error path
	}
	maybeTrimAccessLog(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(lines.String())
}

// sanitizeField makes a value single-line, bounded, delimiter-free: "--" becomes an em dash so a
// crafted value can't shift fields.
func sanitizeField(s string, max int) string {
	s = strings.ReplaceAll(s, "--", "—")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

// maybeTrimAccessLog bounds the log: past the size cap, atomically rewrite keeping only the newest
// lines fitting HALF the byte cap — the hysteresis keeps read+rewrite amortized O(1) between trims.
func maybeTrimAccessLog(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= accessLogMaxByte {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	keep, size := len(lines), 0 // keep = index of the first kept (newest-tail) line
	for keep > 0 && len(lines)-keep < accessLogKeep {
		cost := len(lines[keep-1]) + 1
		if size+cost > accessLogMaxByte/2 {
			break
		}
		size += cost
		keep--
	}
	_ = paths.AtomicWrite(path, []byte(strings.Join(lines[keep:], "\n")+"\n"), 0o600)
}

// AccessStat is one entry's recall profile: total accesses, distinct UTC days within the caller's
// window (0 when no window given), and the newest access.
type AccessStat struct {
	Count  int
	Days   int
	Newest time.Time
}

// AccessStats parses dir's access log into entry → AccessStat (Days = distinct UTC days in
// (now-windowDays, now]). 2nd return is the OLDEST timestamp — the cold-entry gate needs log > horizon.
func AccessStats(dir string, now time.Time, windowDays int) (map[string]AccessStat, time.Time) {
	data, err := os.ReadFile(AccessLogPath(dir))
	if err != nil {
		return map[string]AccessStat{}, time.Time{}
	}
	windowStart := time.Time{}
	if windowDays > 0 && !now.IsZero() {
		windowStart = now.AddDate(0, 0, -windowDays)
	}
	stats := map[string]AccessStat{}
	days := map[string]map[string]bool{} // entry → set of in-window UTC days
	oldest := time.Time{}
	for line := range strings.SplitSeq(string(data), "\n") {
		parts := strings.SplitN(line, " -- ", 3)
		if len(parts) < 2 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, parts[0])
		if err != nil {
			continue
		}
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
		entry := parts[1]
		s := stats[entry]
		s.Count++
		if ts.After(s.Newest) {
			s.Newest = ts
		}
		if !windowStart.IsZero() && ts.After(windowStart) {
			if days[entry] == nil {
				days[entry] = map[string]bool{}
			}
			days[entry][ts.UTC().Format("2006-01-02")] = true
		}
		s.Days = len(days[entry])
		stats[entry] = s
	}
	return stats, oldest
}

// AccessLogPath is dir's recall trace: state/harness/memory/access/<key>.log for a store dir (state, never
// memory history), else access.log beside the bank.
func AccessLogPath(dir string) string {
	if rel, ok := InsideStore(dir); ok {
		key := strings.ReplaceAll(rel, string(filepath.Separator), "_")
		if key == "" || key == "." {
			key = "home"
		}
		return filepath.Join(AccessStateDir(), key+".log")
	}
	return filepath.Join(dir, AccessLogFile)
}
