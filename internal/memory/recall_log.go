package memory

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
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
	slices.Sort(noms) // map iteration order is random; doctor output must be stable
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

const (
	recallIndexFile           = "recall.idx"
	recallIndexMagic          = "CCRI"
	recallIndexVersion uint32 = 2  // bump on any encode or ref change the encoderKey samples cannot see
	indexHeaderLen            = 20 // magic, version, encoder key, entry count
	indexTrailerLen           = 4  // CRC-32C of every byte before it
)

var indexCRC = crc32.MakeTable(crc32.Castagnoli)

// encoderKey fingerprints the encoder's output on samples, so an encoder change misses the index on its own.
var encoderKey = sync.OnceValue(func() uint64 {
	sum := sha256.New()
	for _, c := range []struct {
		mode segMode
		base string
		text string
	}{
		{segEntries, "decisionLog.md", "# Log\n\n## 2026-01-01 — Subagent-Router retry\n" +
			"the and of `fenced` <!-- id:0a1b2c3d -->\n```\n## fenced marker\n```\n\n## héllo wörld 42\nplain body\n"},
		{segEntries, "conventions.md", "# Conventions\n\n- commit small, review focused\n  continued line\n- second rule\n"},
		{segNative, "topic.md", "---\nname: Native Topic\ndescription: router context\n---\nbody text here\n"},
		{segEntries, "progress.md", "# P\n\n## One <!-- id:aaaa0001 --> <!-- id:bbbb0002 -->\n" +
			"text with `tick` and snake_case\n```go\n## not a heading <!-- id:cccc0003 -->\nx := 1\n```\ntail\n"},
		{segKnowledge, "note.md", "---\nname: n\n---\n# Knowledge\nbounded latency\n"},
	} {
		sum.Write(encodeSegment(parseBlocks(c.mode, c.base, []byte(c.text))))
	}
	return binary.LittleEndian.Uint64(sum.Sum(nil))
})

type fileKey struct {
	path string
	mode segMode
}
type indexed struct {
	size, mtimeNs int64
	seg           segView
}

// readRecallIndex maps and decodes the entries in want (all when nil); segments alias it, so release comes last.
func readRecallIndex(path string, key uint64, want map[fileKey]bool) (map[fileKey]indexed, func()) {
	data, release, err := paths.ReadMapped(path)
	if err != nil {
		return make(map[fileKey]indexed), func() {}
	}
	return decodeRecallIndex(data, key, want), release
}

// decodeRecallIndex validates data (magic, version, key, CRC, segments) and decodes it; invalid entries are left out.
func decodeRecallIndex(data []byte, key uint64, want map[fileKey]bool) map[fileKey]indexed {
	if len(data) < indexHeaderLen+indexTrailerLen {
		return make(map[fileKey]indexed)
	}
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	body, trailer := data[:len(data)-indexTrailerLen], data[len(data)-indexTrailerLen:]
	if string(body[:4]) != recallIndexMagic || u32(body[4:]) != recallIndexVersion || binary.LittleEndian.Uint64(body[8:]) != key {
		return make(map[fileKey]indexed)
	}
	sumOK := make(chan bool, 1) // the checksum runs beside segment validation; its verdict gates the result
	go func() { sumOK <- crc32.Checksum(body, indexCRC) == u32(trailer) }()
	m, ok := readIndexEntries(body[indexHeaderLen:], u32(body[16:]), want)
	sumGood := <-sumOK // always joined: the goroutine reads the mapping, which may be released once this returns
	if !ok || !sumGood {
		return make(map[fileKey]indexed)
	}
	return m
}

// retained is the one mapping loadSegments keeps for the process, because the segments it hands out alias it.
var retained struct {
	sync.Mutex
	path string
	info os.FileInfo
	data []byte
}

// retainedIndexData maps path once per file version; a replaced index leaves the old mapping alive for live segments.
func retainedIndexData(path string) []byte {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	retained.Lock()
	defer retained.Unlock()
	if retained.path == path && retained.info != nil && os.SameFile(retained.info, info) &&
		retained.info.Size() == info.Size() && retained.info.ModTime().Equal(info.ModTime()) {
		return retained.data
	}
	data, _, err := paths.ReadMapped(path)
	if err != nil {
		return nil
	}
	retained.path, retained.info, retained.data = path, info, data
	return data
}

// readIndexEntries decodes count entries; false on mis-framing or trailing bytes, and a bad wanted segment is a miss.
func readIndexEntries(data []byte, count uint32, want map[fileKey]bool) (map[fileKey]indexed, bool) {
	var keys []fileKey
	var entries []indexed
	for range count {
		key, entry, rest, ok := readIndexEntry(data)
		if !ok {
			return nil, false
		}
		data = rest
		if want != nil && !want[key] || key.mode > segKnowledge {
			continue
		}
		keys = append(keys, key)
		entries = append(entries, entry)
	}
	if len(data) != 0 {
		return nil, false
	}
	valid := make([]bool, len(entries)) // each segment is validated on the pool, one slot per entry
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	forEachIndex(order, func(i int) { valid[i] = entries[i].seg.Valid() })
	m := make(map[fileKey]indexed, len(entries))
	for i, key := range keys {
		if valid[i] {
			m[key] = entries[i]
		}
	}
	return m, true
}

// readIndexEntry frames one entry from the front of data and returns the bytes after it; the segment is not validated.
func readIndexEntry(data []byte) (key fileKey, entry indexed, rest []byte, ok bool) {
	u32 := func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	u64 := func(b []byte) int64 { return int64(binary.LittleEndian.Uint64(b)) }
	if len(data) < 4 {
		return key, entry, nil, false
	}
	length := uint64(u32(data))
	data = data[4:]
	if length+21 > uint64(len(data)) {
		return key, entry, nil, false
	}
	key = fileKey{string(data[:length]), segMode(data[length])}
	data = data[length+1:]
	entry = indexed{size: u64(data), mtimeNs: u64(data[8:])}
	length = uint64(u32(data[16:]))
	data = data[20:]
	if length > uint64(len(data)) {
		return key, entry, nil, false
	}
	entry.seg = segView(data[:length])
	return key, entry, data[length:], true
}

func writeRecallIndex(path string, key uint64, m map[fileKey]indexed) error {
	keys := make([]fileKey, 0, len(m))
	size := indexHeaderLen + indexTrailerLen
	for fk, entry := range m {
		keys = append(keys, fk)
		size += 25 + len(fk.path) + len(entry.seg)
	}
	slices.SortFunc(keys, func(a, b fileKey) int {
		if a.path != b.path {
			return strings.Compare(a.path, b.path)
		}
		return cmp.Compare(a.mode, b.mode)
	})
	data := append(make([]byte, 0, size), recallIndexMagic...)
	data = binary.LittleEndian.AppendUint32(data, recallIndexVersion)
	data = binary.LittleEndian.AppendUint64(data, key)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(keys)))
	for _, fk := range keys {
		entry := m[fk]
		data = binary.LittleEndian.AppendUint32(data, uint32(len(fk.path)))
		data = append(data, fk.path...)
		data = append(data, byte(fk.mode))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.size))
		data = binary.LittleEndian.AppendUint64(data, uint64(entry.mtimeNs))
		data = binary.LittleEndian.AppendUint32(data, uint32(len(entry.seg)))
		data = append(data, entry.seg...)
	}
	data = binary.LittleEndian.AppendUint32(data, crc32.Checksum(data, indexCRC))
	return paths.AtomicWriteNoSync(path, data, 0o600) // derived cache: a torn file fails the CRC and rebuilds
}

// pruneMissing drops index entries whose file is gone, reusing stats and statting only the unchecked ones.
func pruneMissing(index map[fileKey]indexed, stats map[string]bool) {
	for key := range index {
		exists, checked := stats[key.path]
		if !checked {
			_, err := os.Stat(key.path)
			exists = err == nil
			stats[key.path] = exists
		}
		if !exists {
			delete(index, key)
		}
	}
}

// fresh returns the cached segment when its recorded size and mtime still match info; the zero entry never matches.
func (e indexed) fresh(info os.FileInfo) (segView, bool) {
	if e.seg == nil || e.size != info.Size() || e.mtimeNs != info.ModTime().UnixNano() {
		return nil, false
	}
	return e.seg, true
}

// encodeFile reads and indexes f; nil when it cannot be read.
func encodeFile(f corpusFile) segView {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return nil
	}
	return encodeSegment(parseBlocks(f.Mode, filepath.Base(f.Path), data))
}

// loadSegments is files' segments from the index; with terms and RowRecall set, stale files come from the row store.
func loadSegments(files []corpusFile, persist bool, terms []string) []segView {
	key := encoderKey()
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	index := readFilesIndex(path, key, files)
	segs := make([]segView, len(files))
	// Reuse query stats when pruning so each corpus file is statted only once.
	stats := make(map[string]bool, len(files))
	added := map[fileKey]indexed{}
	var missing []int
	var infos []os.FileInfo
	for i, f := range files {
		info, err := os.Stat(f.Path)
		stats[f.Path] = err == nil
		if err != nil {
			if !os.IsNotExist(err) {
				terms = nil // an unstattable file declines the rows: Markdown is the fallback
			}
			continue
		}
		key := fileKey{f.Path, f.Mode}
		if seg, hit := index[key].fresh(info); hit {
			segs[i] = seg
			continue
		}
		missing = append(missing, i)
		infos = append(infos, info)
	}
	encodeMissing(files, missing, segs, terms)
	for j, i := range missing {
		info := infos[j]
		if segs[i] != nil && persist && time.Since(info.ModTime()) >= paths.RacyWindow {
			added[fileKey{files[i].Path, files[i].Mode}] = indexed{info.Size(), info.ModTime().UnixNano(), segs[i]}
		}
	}
	persistIndex(path, key, added, stats)
	return segs
}

const corpusReadWorkers = 4

// encodeMissing indexes the missing files; given terms, those RowRecall renders are parsed from its rows, not read.
func encodeMissing(files []corpusFile, missing []int, segs []segView, terms []string) {
	if len(missing) == 0 {
		return
	}
	var docs map[string][]byte // nil when RowRecall is unset, unasked or declines: every file is read
	if RowRecall != nil && len(terms) > 0 {
		if rows, ok := RowRecall(terms); ok {
			docs = rows
		}
	}
	store := paths.MemoryStoreDir() + string(filepath.Separator)
	_, err := parallel.Map(context.Background(), missing, corpusReadWorkers, func(_ context.Context, i int) (struct{}, error) {
		rel := strings.TrimPrefix(files[i].Path, store)
		if data, fromRows := docs[filepath.ToSlash(rel)]; fromRows {
			segs[i] = encodeSegment(parseBlocks(files[i].Mode, filepath.Base(files[i].Path), data))
		} else {
			segs[i] = encodeFile(files[i])
		}
		return struct{}{}, nil
	})
	parallel.Rethrow(err)
}

// readFilesIndex is the persisted index restricted to files' (path, mode) keys.
func readFilesIndex(path string, key uint64, files []corpusFile) map[fileKey]indexed {
	want := make(map[fileKey]bool, len(files))
	for _, f := range files {
		want[fileKey{f.Path, f.Mode}] = true
	}
	return decodeRecallIndex(retainedIndexData(path), key, want)
}

// persistIndex merges added into the on-disk index and drops files that no longer exist.
func persistIndex(path string, key uint64, added map[fileKey]indexed, stats map[string]bool) {
	if len(added) == 0 || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	_, _ = paths.TryFileLock(path+".lock", time.Second, func() error { // re-read under the lock: keep peers' entries
		cur, release := readRecallIndex(path, key, nil)
		defer release() // writeRecallIndex copies every segment into its buffer before this runs
		maps.Copy(cur, added)
		pruneMissing(cur, stats)
		return writeRecallIndex(path, key, cur)
	})
}
