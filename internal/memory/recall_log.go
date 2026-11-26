package memory

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unsafe"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// Access reasons distinguishing recall injections from explicit CLI returns.
const (
	accessRecall = "prompt-recall inject"
	AccessSearch = "memory search"
	AccessShow   = "memory show"
)

const (
	accessEntryMax    = 120
	accessReasonMax   = 60
	accessRotateDays  = 30
	accessGenerations = 3
	accessLockWait    = 500 * time.Millisecond
)

// accessLines renders one line per entry sharing timestamp and reason; a session makes "session -- <id> -- " records.
func accessLines(now time.Time, entries []string, reason, session string) string {
	var lines strings.Builder
	prefix := now.UTC().Format(time.RFC3339) + " -- "
	if session != "" { // only the learner reads session records
		prefix = sessionTag + " -- " + session + " -- " + prefix
	}
	suffix := " -- " + sanitizeField(reason, accessReasonMax) + "\n"
	for _, entry := range entries {
		lines.WriteString(prefix)
		lines.WriteString(sanitizeField(entry, accessEntryMax))
		lines.WriteString(suffix)
	}
	return lines.String()
}

// sessionTag opens a session record; an older reader cannot parse it as a timestamp and skips the whole line.
const sessionTag = "session"

// accessLogWritable reports whether path may be appended to: not a planted symlink/oddity, and its dir exists.
func accessLogWritable(path string) bool {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return false // symlink/oddity planted at the log name — never write through it
	}
	// A dir that cannot be created means the log is not scaffolded yet — fail-open, like every other logAccess error path.
	return os.MkdirAll(filepath.Dir(path), 0o755) == nil
}

// logAccessBatch appends a recall batch with one open and write per bank.
func logAccessBatch(now time.Time, entries []string, reason, session string) {
	if len(entries) == 0 {
		return
	}
	if textutil.CredentialRE().MatchString(reason) {
		reason = "(reason redacted — credential-shaped)"
	}
	path := accessLogPath()
	if !accessLogWritable(path) {
		return
	}
	lines := accessLines(now, entries, reason, "")
	write := func() error { return rotateAndAppend(path, now, entries, reason, session, lines) }
	if ran, err := paths.TryFileLock(path+".lock", accessLockWait, write); !ran && err != nil {
		_ = appendAccessLines(path, lines) // lock file unopenable: O_APPEND only, never rotate unlocked; a timeout drops it
	}
}

// rotateAndAppend rotates and appends under one lock so concurrent hooks and verbs never interleave renames.
func rotateAndAppend(path string, now time.Time, entries []string, reason, session, lines string) error {
	maybeRotateAccessLog(path, now)
	session = strings.ToLower(session)
	sessionOK := looksLikeSessionID(session)
	fresh := entries
	if reason == accessRecall { // hot/cold count distinct days, so a repeat inject adds nothing; one scan serves both
		var plain []string
		plain, fresh = newToday(path, now, entries, session, sessionOK)
		lines = accessLines(now, plain, accessRecall, "")
	}
	if sessionOK { // one record per session, id and day is all the learner needs
		lines += accessLines(now, fresh, reason, session)
	}
	if lines == "" {
		return nil
	}
	return appendAccessLines(path, lines)
}

// newToday drops entries (and repeats) path holds as a recall inject on now's UTC day: plain, and session's.
func newToday(path string, now time.Time, entries []string, session string, withSession bool) (plain, sess []string) {
	day := now.UTC().Format("2006-01-02")
	seen, seenSess := map[string]bool{}, map[string]bool{}
	eachParsedLine(path, func(ts time.Time, entry, reason, s string) {
		if reason != accessRecall || ts.UTC().Format("2006-01-02") != day {
			return
		}
		if s == "" {
			seen[entry] = true
		} else if withSession && s == session {
			seenSess[entry] = true
		}
	}, accessRecall)
	fresh := func(seen map[string]bool) (out []string) { // keyed by the sanitized log form, repeats dropped too
		for _, e := range entries {
			if key := sanitizeField(e, accessEntryMax); !seen[key] {
				seen[key] = true
				out = append(out, e)
			}
		}
		return out
	}
	if withSession {
		sess = fresh(seenSess)
	}
	return fresh(seen), sess
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

// eachAccessLine calls fn per plain line of path's generations, oldest first, then the active log; see eachParsedLine.
func eachAccessLine(path string, fn func(ts time.Time, entry, reason string), reasons ...string) {
	for i := accessGenerations; i >= 0; i-- {
		eachParsedLine(accessGeneration(path, i), func(ts time.Time, entry, reason, session string) {
			if session == "" { // session records are the learner's alone
				fn(ts, entry, reason)
			}
		}, reasons...)
	}
}

func accessGeneration(path string, i int) string {
	if i == 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, i)
}

func parseAccessLine(line string) (ts time.Time, entry, reason, session string) {
	stamp, fields, ok := strings.Cut(line, " -- ")
	if !ok {
		return
	}
	if stamp == sessionTag {
		session, fields, _ = strings.Cut(fields, " -- ")
		stamp, fields, _ = strings.Cut(fields, " -- ")
	}
	ts, _ = time.Parse(time.RFC3339, stamp)
	entry, reason, _ = strings.Cut(fields, " -- ")
	return ts, entry, strings.TrimSpace(reason), session
}

// LogBriefRecall credits only the pointers that survived dispatch filtering.
func LogBriefRecall(session, block string) {
	if !looksLikeSessionID(session) {
		return
	}
	var ids []string
	for line := range strings.SplitSeq(block, "\n") {
		if pointer, ok := strings.CutPrefix(line, "- "); ok {
			id, _, _ := strings.Cut(pointer, " ")
			if idRef().MatchString(id) {
				ids = append(ids, id)
			}
		}
	}
	logAccessBatch(time.Now(), ids, accessRecall, session)
}

// eachParsedLine calls fn per parsed line of p; given reasons, a line whose tail ends in none is skipped unparsed.
func eachParsedLine(p string, fn func(ts time.Time, entry, reason, session string), reasons ...string) {
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return
	}
	text := unsafe.String(&data[0], len(data)) // data is never written again, so the string needs no copy
	for line := range strings.SplitSeq(text, "\n") {
		if len(reasons) > 0 && !slices.ContainsFunc(reasons, func(r string) bool {
			return strings.HasSuffix(strings.TrimRightFunc(line, unicode.IsSpace), r)
		}) {
			continue // parseAccessLine's reason is the trimmed tail, so a line without one could never match
		}
		if ts, entry, reason, session := parseAccessLine(line); !ts.IsZero() {
			fn(ts, entry, reason, session)
		}
	}
}

// returnCounts counts searches and shows of the last 30 days; recall injections excluded.
func returnCounts(now time.Time) map[string]int {
	counts := map[string]int{}
	cutoff := now.AddDate(0, 0, -accessRotateDays)
	eachAccessLine(accessLogPath(), func(ts time.Time, entry, reason string) {
		if ts.After(cutoff) && (reason == AccessSearch || reason == AccessShow) {
			counts[entry]++
		}
	}, AccessSearch, AccessShow)
	return counts
}

// logHits logs access for the hits that carry an entry id, all in the one bank's log.
func logHits(hits []SearchHit, reason string, now time.Time, session string) {
	var ids []string
	for _, h := range hits {
		if id := cmp.Or(h.ID, entryFromHit(h).ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		logAccessBatch(now, ids, reason, session)
	}
}

// logShown logs access for a single shown block.
func logShown(id, reason string, now time.Time) {
	if id == "" {
		return
	}
	logAccessBatch(now, []string{id}, reason, os.Getenv(paths.SessionIDEnv))
}

// accessLogPath is the one bank's access trace, outside the store.
func accessLogPath() string { return filepath.Join(AccessStateDir(), stateKey()+".log") }

// Access-driven nomination thresholds, computed from real recall data.
const (
	hotWindowDays = 30 // recall window for the hot check
	hotMinDays    = 3  // distinct in-window days that make an entry "hot"
	coldDays      = 90 // no recall for this long → archive candidate (log-age gated)
)

// accessNominations suggests pointers for hot entries and archives for cold entries with enough log history.
func accessNominations(now time.Time) []string {
	files := transcript.GlobIn(AccessStateDir(), "*.log")
	if len(files) == 0 {
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

const recallMinInjections = 5

type recallTally struct {
	Injected int `json:"injected"`
	Used     int `json:"used"`
}

type recallLearning struct {
	Offsets map[string]int64                     `json:"offsets"`
	Days    map[string]map[string]recallTally    `json:"days"`
	Pending map[string]map[string]map[string]int `json:"pending"`
}

func recallLearningPath() string {
	return filepath.Join(paths.MemoryStateDir(), "recall-learning.json")
}

var learnedBoosts = paths.NewStampCache[map[string]float64]("memory.learnedboosts")

func recallWeights() map[string]float64 {
	path := recallLearningPath()
	weights, _ := learnedBoosts.Get(path, func() (map[string]float64, error) {
		state, _, err := loadRecallLearning(path)
		if err != nil {
			return nil, err
		}
		return state.weights(), nil
	})
	return weights
}

func (s recallLearning) weights() map[string]float64 {
	totals := map[string]recallTally{}
	for _, entries := range s.Days {
		for id, t := range entries {
			sum := totals[id]
			totals[id] = recallTally{sum.Injected + t.Injected, sum.Used + t.Used}
		}
	}
	weights := make(map[string]float64, len(totals))
	for id, t := range totals {
		if t.Injected >= recallMinInjections {
			weights[id] = min(1.2, max(0.8, 0.8+0.4*float64(t.Used)/float64(t.Injected)))
		}
	}
	return weights
}

func loadRecallLearning(path string) (recallLearning, []byte, error) {
	s := recallLearning{map[string]int64{}, map[string]map[string]recallTally{}, map[string]map[string]map[string]int{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	return s, data, err
}

// LearnRecall checkpoints the access generations so a rename never counts a delivery twice.
func LearnRecall(now time.Time) error {
	log := accessLogPath()
	if _, err := os.Stat(filepath.Dir(log)); os.IsNotExist(err) {
		return nil
	}
	ran, err := paths.TryFileLock(log+".lock", accessLockWait, func() error {
		return learnRecallLocked(log, recallLearningPath(), now.UTC())
	})
	if !ran && err == nil {
		return paths.ErrLockTimeout
	}
	return err
}

func learnRecallLocked(log, path string, now time.Time) error {
	s, previous, err := loadRecallLearning(path)
	if err != nil {
		return err
	}
	start := now.AddDate(0, 0, 1-accessRotateDays).Format(time.DateOnly)
	s.expire(start)
	offsets := map[string]int64{}
	for i := accessGenerations; i >= 0; i-- {
		key, end, err := s.consume(accessGeneration(log, i), start)
		if err != nil {
			return err
		}
		if key != "" {
			offsets[key] = end
		}
	}
	s.Offsets = offsets
	if previous == nil && len(s.Days) == 0 {
		return nil
	}
	data, err := json.Marshal(s)
	if err != nil || bytes.Equal(data, previous) {
		return err
	}
	return paths.AtomicWrite(path, data, 0o600)
}

func (s *recallLearning) expire(start string) {
	for day := range s.Days {
		if day < start {
			delete(s.Days, day)
			delete(s.Pending, day)
		}
	}
}

func (s *recallLearning) consume(path, start string) (string, int64, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	first, err := r.ReadString('\n')
	if err == io.EOF {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256([]byte(first))
	key := hex.EncodeToString(digest[:])
	offset := s.Offsets[key]
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", 0, err
	}
	r.Reset(f)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			return key, offset, nil
		}
		if err != nil {
			return "", 0, err
		}
		s.observe(strings.TrimSuffix(line, "\n"), start)
		offset += int64(len(line))
	}
}

func (s *recallLearning) observe(line, start string) {
	ts, id, reason, session := parseAccessLine(line)
	if ts.IsZero() || !looksLikeSessionID(session) || id == "" {
		return
	}
	day := ts.UTC().Format(time.DateOnly)
	if day < start {
		return
	}
	switch reason {
	case accessRecall:
		s.inject(day, id, session)
	case AccessShow, AccessSearch:
		for day, sessions := range s.Pending {
			if ids := sessions[session]; ids[id] > 0 {
				t := s.Days[day][id]
				t.Used += ids[id]
				s.Days[day][id] = t
				delete(ids, id)
				if len(ids) == 0 {
					delete(sessions, session)
				}
			}
		}
	}
}

func (s *recallLearning) inject(day, id, session string) {
	if s.Days[day] == nil {
		s.Days[day] = map[string]recallTally{}
		s.Pending[day] = map[string]map[string]int{}
	}
	t := s.Days[day][id]
	t.Injected++
	s.Days[day][id] = t
	if s.Pending[day][session] == nil {
		s.Pending[day][session] = map[string]int{}
	}
	s.Pending[day][session][id]++
}

func learnRecallForIndex(errw io.Writer) {
	if err := LearnRecall(time.Now()); err != nil {
		fmt.Fprintf(errw, "memory index: recall learning: %v\n", err)
	}
}
