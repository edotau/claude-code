package memory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// accessLogFile is the append-only access trace; it never enters the injected bank.
const accessLogFile = "access.log"

// Access reasons distinguishing recall injections from explicit CLI returns.
const (
	accessRecall = "prompt-recall inject"
	AccessSearch = "memory search"
	AccessShow   = "memory show"
)

const (
	accessEntryMax    = 120
	accessReasonMax   = 60
	accessLogMaxByte  = 256 << 10
	accessRetainDays  = 120 // lines older than this are dropped when the size cap trims
	accessRotateDays  = 30
	accessGenerations = 3
	accessLockWait    = 500 * time.Millisecond
)

// logAccess appends one read to dir's log, failing open and redacting credential-shaped reasons.
func logAccess(dir string, now time.Time, entry, reason string) {
	logAccessBatch(dir, now, []string{entry}, reason)
}

// accessLines renders one log line per entry, sharing the timestamp and sanitized reason.
func accessLines(now time.Time, entries []string, reason string) string {
	var lines strings.Builder
	prefix := now.UTC().Format(time.RFC3339) + " -- "
	suffix := " -- " + sanitizeField(reason, accessReasonMax) + "\n"
	for _, entry := range entries {
		lines.WriteString(prefix + sanitizeField(entry, accessEntryMax) + suffix)
	}
	return lines.String()
}

// accessLogWritable reports whether path may be appended to: not a planted symlink/oddity, and its dir exists.
func accessLogWritable(path string) bool {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return false // symlink/oddity planted at the log name — never write through it
	}
	// A dir that cannot be created means the log is not scaffolded yet — fail-open, like every other logAccess error path.
	return os.MkdirAll(filepath.Dir(path), 0o755) == nil
}

// logAccessBatch appends a recall batch with one open and write per bank.
func logAccessBatch(dir string, now time.Time, entries []string, reason string) {
	if dir == "" || len(entries) == 0 {
		return
	}
	if textutil.CredentialRE.MatchString(reason) {
		reason = "(reason redacted — credential-shaped)"
	}
	path := AccessLogPath(dir)
	if !accessLogWritable(path) {
		return
	}
	lines := accessLines(now, entries, reason)
	// Rotate, trim and append under one lock: concurrent hooks and verbs must never interleave the renames.
	write := func() error {
		maybeRotateAccessLog(path, now)
		maybeTrimAccessLog(path, now)
		l := lines
		if reason == accessRecall { // hot/cold count distinct days, so a repeat inject adds nothing
			l = accessLines(now, newToday(path, now, entries), reason)
		}
		if l == "" {
			return nil
		}
		return appendAccessLines(path, l)
	}
	if ran, err := paths.TryFileLock(path+".lock", accessLockWait, write); !ran && err != nil {
		_ = appendAccessLines(path, lines) // lock file unopenable: O_APPEND only, never rotate or trim unlocked; a timeout drops it
	}
}

// newToday drops entries (and repeats within entries) that path already holds as a recall inject on now's UTC day.
func newToday(path string, now time.Time, entries []string) []string {
	day := now.UTC().Format("2006-01-02")
	seen := map[string]bool{}
	eachLineIn(path, func(ts time.Time, entry, reason string) {
		if reason == accessRecall && ts.UTC().Format("2006-01-02") == day {
			seen[entry] = true
		}
	})
	var fresh []string
	for _, e := range entries {
		key := sanitizeField(e, accessEntryMax)
		if !seen[key] {
			seen[key] = true
			fresh = append(fresh, e)
		}
	}
	return fresh
}

func appendAccessLines(path, lines string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lines)
	return err
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

// sanitizeField bounds a value to one line and replaces delimiters so input cannot shift fields.
func sanitizeField(s string, max int) string {
	s = strings.ReplaceAll(s, "--", "—")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

// maybeTrimAccessLog drops lines older than accessRetainDays once the byte cap is exceeded.
func maybeTrimAccessLog(path string, now time.Time) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= accessLogMaxByte || now.IsZero() {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -accessRetainDays)
	var kept strings.Builder
	dropped := false
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		ts, err := time.Parse(time.RFC3339, strings.SplitN(line, " -- ", 2)[0])
		if err != nil || ts.Before(cutoff) {
			dropped = true
			continue
		}
		kept.WriteString(line + "\n")
	}
	if dropped {
		_ = paths.AtomicWrite(path, []byte(kept.String()), 0o600)
	}
}

// accessStat holds an entry's distinct in-window UTC days and newest access.
type accessStat struct {
	Days   int
	Newest time.Time
}

// accessStatsAt folds rotated logs into entry counts and returns the oldest timestamp for the cold-entry gate.
func accessStatsAt(path string, now time.Time, windowDays int) (map[string]accessStat, time.Time) {
	windowStart := time.Time{}
	if windowDays > 0 && !now.IsZero() {
		windowStart = now.AddDate(0, 0, -windowDays)
	}
	stats := map[string]accessStat{}
	days := map[string]map[string]bool{} // entry → set of in-window UTC days
	oldest := time.Time{}
	eachAccessLine(path, func(ts time.Time, entry, _ string) {
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
		s := stats[entry]
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
		eachLineIn(p, fn)
	}
}

// eachLineIn calls fn per parsed line of the single log file p.
func eachLineIn(p string, fn func(ts time.Time, entry, reason string)) {
	data, err := os.ReadFile(p)
	if err != nil {
		return
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

// returnCounts counts per entry the searches and shows of the last 30 days in dirs' logs; recall injections excluded.
func returnCounts(dirs []string, now time.Time) map[string]int {
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

// logHits logs access for matching search hits grouped by each hit's owning bank directory.
func logHits(root string, hits []SearchHit, reason string, now time.Time) {
	if len(hits) == 0 {
		return
	}
	grouped := map[string][]string{}
	for _, h := range hits {
		key := h.ID
		if key == "" {
			key = entryFromHit(h).ID
		}
		if key == "" {
			continue
		}
		dir := h.Dir
		if dir == "" {
			switch h.Bank {
			case "local":
				dir = Dir(root)
			case "home", "native", "archive", "knowledge", "":
				dir = HomeBankDir()
			default:
				dir = filepath.Join(StoreDir(), h.Bank)
			}
		}
		grouped[dir] = append(grouped[dir], key)
	}
	for dir, entries := range grouped {
		logAccessBatch(dir, now, entries, reason)
	}
}

// logShown logs access for a single shown block.
func logShown(path, id, reason string, now time.Time) {
	if id == "" {
		return
	}
	dir := HomeBankDir()
	if rel, ok := insideStore(path); ok {
		if first, _, _ := strings.Cut(rel, "/"); first != "" && first != "." {
			dir = filepath.Join(StoreDir(), first)
		}
	}
	logAccess(dir, now, id, reason)
}

// AccessLogPath puts store traces in state/harness/memory/access, otherwise beside the bank.
func AccessLogPath(dir string) string {
	if rel, ok := insideStore(dir); ok {
		key := strings.ReplaceAll(rel, "/", "_")
		if key == "" || key == "." {
			key = "home"
		}
		return filepath.Join(AccessStateDir(), key+".log")
	}
	return filepath.Join(dir, accessLogFile)
}

// Access-driven nomination thresholds, computed from real recall data.
const (
	hotWindowDays = 30 // recall window for the hot check
	hotMinDays    = 3  // distinct in-window days that make an entry "hot"
	coldDays      = 90 // no recall for this long → archive candidate (log-age gated)
)

// accessNominations suggests pointers for hot entries and archives for cold entries with enough log history.
func accessNominations(now time.Time) []string {
	files, err := filepath.Glob(filepath.Join(AccessStateDir(), "*.log"))
	if err != nil || len(files) == 0 {
		return nil
	}
	var noms []string
	for _, f := range files {
		key := strings.TrimSuffix(filepath.Base(f), ".log")
		stats, oldest := accessStatsAt(f, now, hotWindowDays)
		if oldest.IsZero() {
			continue
		}
		logCoversCold := oldest.Before(now.AddDate(0, 0, -coldDays))
		for entry, s := range stats {
			if s.Days >= hotMinDays {
				noms = append(noms, fmt.Sprintf(
					"%s/%s: hot — recalled on %d days in the last %dd; consider a pointer in the injected bank",
					key, entry, s.Days, hotWindowDays))
				continue
			}
			if logCoversCold && s.Newest.Before(now.AddDate(0, 0, -coldDays)) {
				noms = append(noms, fmt.Sprintf(
					"%s/%s: not recalled in %dd — archive candidate", key, entry, coldDays))
			}
		}
	}
	sort.Strings(noms) // map iteration order is random; doctor output must be stable
	return noms
}
