package recall

import (
	"bufio"
	"fmt"
	"github.com/edotau/claude-code/internal/memory/bank"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// AccessLogFile is the append-only access trace; it never enters the injected bank.
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
	accessLockWait    = 500 * time.Millisecond
)

// LogAccess appends one read to dir's log, failing open and redacting credential-shaped reasons.
func LogAccess(dir string, now time.Time, entry, reason string) {
	LogAccessBatch(dir, now, []string{entry}, reason)
}

// LogAccessBatch appends a recall batch with one open and write per bank.
func LogAccessBatch(dir string, now time.Time, entries []string, reason string) {
	if dir == "" || len(entries) == 0 {
		return
	}
	if bank.CredentialPattern.MatchString(reason) {
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
	// Rotate, trim and append under one lock: concurrent hooks and verbs must never interleave the renames.
	write := func() error {
		maybeRotateAccessLog(path, now)
		maybeTrimAccessLog(path)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(lines.String())
		return err
	}
	if ran, err := paths.TryFileLock(path+".lock", accessLockWait, write); !ran && err != nil {
		_ = write() // lock file unopenable: append unlocked rather than drop the line; a timeout drops it
	}
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

// sanitizeField bounds a value to one line and replaces delimiters so input cannot shift fields.
func sanitizeField(s string, max int) string {
	s = strings.ReplaceAll(s, "--", "—")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

// maybeTrimAccessLog keeps the newest lines within half the byte cap once the full cap is exceeded.
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

// AccessStat counts an entry's reads, distinct in-window UTC days and newest access.
type AccessStat struct {
	Count  int
	Days   int
	Newest time.Time
}

// accessStatsAt folds rotated logs into entry counts and returns the oldest timestamp for the cold-entry gate.
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
func LogHits(root string, hits []bank.SearchHit, reason string, now time.Time) {
	if len(hits) == 0 {
		return
	}
	grouped := map[string][]string{}
	for _, h := range hits {
		key := h.ID
		if key == "" {
			key = bank.EntryFromHit(h).ID
		}
		if key == "" {
			continue
		}
		dir := h.Dir
		if dir == "" {
			switch h.Bank {
			case "local":
				dir = memstore.Dir(root)
			case "home", "native", "archive", "knowledge", "":
				dir = memstore.HomeBankDir()
			default:
				dir = filepath.Join(memstore.StoreDir(), h.Bank)
			}
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
	dir := memstore.HomeBankDir()
	if rel, ok := memstore.InsideStore(path); ok {
		if first, _, _ := strings.Cut(rel, "/"); first != "" && first != "." {
			dir = filepath.Join(memstore.StoreDir(), first)
		}
	}
	LogAccess(dir, now, id, reason)
}

// AccessLogPath puts store traces in state/harness/memory/access, otherwise beside the bank.
func AccessLogPath(dir string) string {
	if rel, ok := memstore.InsideStore(dir); ok {
		key := strings.ReplaceAll(rel, string(filepath.Separator), "_")
		if key == "" || key == "." {
			key = "home"
		}
		return filepath.Join(memstore.AccessStateDir(), key+".log")
	}
	return filepath.Join(dir, AccessLogFile)
}
