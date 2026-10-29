package memory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// AccessLogFile is the append-only recall trace beside a bank: one line per read, the signal
// `AccessNominations` reads. Bank-adjacent beacon — NOT a typed memory file; never injected.
const AccessLogFile = "access.log"

// Access reasons distinguishing recall injections from explicit CLI returns.
const (
	AccessRecall = "prompt-recall inject"
	AccessSearch = "memory search"
	AccessShow   = "memory show"
)

const (
	accessEntryMax    = 120
	accessReasonMax   = 60
	accessLogMaxByte  = 256 << 10
	accessLogKeep     = 2000 // newest lines kept when the size cap trims
	accessRotateDays  = 30
	accessGenerations = 3
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
	maybeRotateAccessLog(path, now)
	maybeTrimAccessLog(path)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(lines.String())
}

// maybeRotateAccessLog rotates the active log when its first entry is older than accessRotateDays.
func maybeRotateAccessLog(path string, now time.Time) {
	if now.IsZero() {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return
	}
	parts := strings.SplitN(scanner.Text(), " -- ", 2)
	if len(parts) == 0 {
		return
	}
	ts, err := time.Parse(time.RFC3339, parts[0])
	if err != nil || !ts.Before(now.AddDate(0, 0, -accessRotateDays)) {
		return
	}
	_ = f.Close()
	for i := accessGenerations - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	_ = os.Rename(path, path+".1")
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
	return accessStatsAt(AccessLogPath(dir), now, windowDays)
}

// accessStatsAt parses the log path and its rotated generations (.3, .2, .1, active).
func accessStatsAt(path string, now time.Time, windowDays int) (map[string]AccessStat, time.Time) {
	windowStart := time.Time{}
	if windowDays > 0 && !now.IsZero() {
		windowStart = now.AddDate(0, 0, -windowDays)
	}
	stats := map[string]AccessStat{}
	days := map[string]map[string]bool{} // entry → set of in-window UTC days
	oldest := time.Time{}
	eachAccessLine(path, func(ts time.Time, entry, _ string) {
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
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
	})
	return stats, oldest
}

// eachAccessLine calls fn per parsed line of path's generations, oldest first, then the active log.
func eachAccessLine(path string, fn func(ts time.Time, entry, reason string)) {
	for i := accessGenerations; i >= 0; i-- {
		p := path
		if i > 0 {
			p = fmt.Sprintf("%s.%d", path, i)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			parts := strings.SplitN(line, " -- ", 3)
			if len(parts) < 2 {
				continue
			}
			ts, err := time.Parse(time.RFC3339, parts[0])
			if err != nil {
				continue
			}
			reason := ""
			if len(parts) == 3 {
				reason = strings.TrimSpace(parts[2])
			}
			fn(ts, parts[1], reason)
		}
	}
}

// ReturnCounts counts per entry the searches and shows of the last 30 days in dirs' logs; recall injections excluded.
func ReturnCounts(dirs []string, now time.Time) map[string]int {
	counts := map[string]int{}
	cutoff := now.AddDate(0, 0, -accessRotateDays)
	seen := map[string]bool{}
	for _, dir := range dirs {
		path := AccessLogPath(dir)
		if dir == "" || seen[path] {
			continue
		}
		seen[path] = true
		eachAccessLine(path, func(ts time.Time, entry, reason string) {
			if ts.After(cutoff) && (reason == AccessSearch || reason == AccessShow) {
				counts[entry]++
			}
		})
	}
	return counts
}

// LogHits logs access for matching search hits grouped by each hit's owning bank directory.
func LogHits(root string, hits []SearchHit, reason string, now time.Time) {
	if len(hits) == 0 {
		return
	}
	grouped := map[string][]string{}
	for _, h := range hits {
		key := EntryFromHit(h).ID
		if key == "" {
			continue
		}
		var dir string
		switch h.Bank {
		case "local":
			dir = Dir(root)
		case "home", "native", "archive", "knowledge", "":
			dir = HomeBankDir()
		default:
			dir = filepath.Join(StoreDir(), h.Bank)
		}
		grouped[dir] = append(grouped[dir], key)
	}
	for dir, entries := range grouped {
		LogAccessBatch(dir, now, entries, reason)
	}
}

// LogShown logs access for a single shown block.
func LogShown(path, id, reason string, now time.Time) {
	if id == "" {
		return
	}
	dir := HomeBankDir()
	if rel, ok := InsideStore(path); ok {
		if first, _, _ := strings.Cut(rel, "/"); first != "" && first != "." {
			dir = filepath.Join(StoreDir(), first)
		}
	}
	LogAccess(dir, now, id, reason)
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
