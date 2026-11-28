package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// Rotation preserves history; returns exclude old reads and injections.
func TestAccessRotationReturnCountsAndNominations(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -(accessRotateDays + 1))
	logAccess(old, "aa000001", accessRecall)
	logAccess(old, "cc000003", AccessSearch)
	logAccess(now, "bb000002", accessRecall) // the write that must rotate
	logPath := accessLogPath()
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("a first entry older than %dd did not rotate to .1: %v", accessRotateDays, err)
	}
	stats, oldest := accessStatsAt(accessLogPath(), now, 0)
	if !oldest.Equal(old) {
		t.Errorf("rotation lost the cold horizon: oldest = %v, want %v", oldest, old)
	}
	if s := stats["aa000001"]; s.Newest.IsZero() {
		t.Errorf("the rotated entry must still count: %+v", s)
	}
	// returnCounts: search/show returns inside the window count; recall injections and old lines never do.
	logAccess(now.AddDate(0, 0, -10), "cc000003", AccessSearch)
	logAccess(now.AddDate(0, 0, -5), "cc000003", AccessShow)
	logAccess(now.AddDate(0, 0, -2), "cc000003", accessRecall)
	counts := returnCounts(now)
	if counts["cc000003"] != 2 {
		t.Errorf("returnCounts[cc000003] = %d, want 2 (in-window search + show)", counts["cc000003"])
	}
	if counts["aa000001"] != 0 || counts["bb000002"] != 0 {
		t.Errorf("recall injections must never count as returns: %v", counts)
	}

	assertHotNomination(t, now)
}

// Nominations prefix entries with the home bank's key.
func assertHotNomination(t *testing.T, now time.Time) {
	t.Helper()
	for d := range hotMinDays {
		logAccess(now.AddDate(0, 0, -d), "dd000004", accessRecall)
	}
	want := "home/dd000004: hot"
	if !slices.ContainsFunc(accessNominations(now), func(n string) bool { return strings.HasPrefix(n, want) }) {
		t.Errorf("want a hot nomination %q, got %v", want, accessNominations(now))
	}
}

// Regression: concurrent writers that all see a stale first line rotate once, and no line is lost.
func TestAccessLogConcurrentRotationRotatesOnce(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	logAccess(now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", accessRecall)
	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { logAccess(now, fmt.Sprintf("bb%06d", i), AccessSearch) })
	}
	wg.Wait()
	path := accessLogPath()
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("concurrent writers rotated more than once")
	}
	if n := len(returnCounts(now)); n != writers {
		t.Fatalf("%d of %d concurrent lines survived", n, writers)
	}
}

// Every hit, wherever its file sits, is credited to the one bank's access log.
func TestLogHitsCreditsTheOneBank(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	write(t, paths.MemorySub("archive", "old.md"), "# A\n\n## 2026-01-01 — zebra archived quota <!-- id:bbbb0001 -->\n\nzebra quota\n")
	write(t, Path("decisionLog.md"), "# D\n\n## 2026-01-01 — zebra live quota <!-- id:bbbb0003 -->\n\nzebra quota\n")
	now := time.Now()
	logHits(SearchBank("zebra quota", 0), AccessSearch, now, "")
	got := returnCounts(now)
	for _, id := range []string{"bbbb0001", "bbbb0003"} {
		if got[id] == 0 {
			t.Errorf("%s not credited to the one bank: %v", id, got)
		}
	}
}

func TestLogAccessAndAccessStatsAt(t *testing.T) {
	tempConfig(t)
	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	logAccess(day1, "aa11bb22", accessRecall)
	logAccess(day1.AddDate(0, 0, 1), "aa11bb22", accessRecall)
	logAccess(day1.AddDate(0, 0, 2), "aa11bb22", accessRecall)
	logAccess(day1, "cc33dd44", accessRecall)

	stats, oldest := accessStatsAt(accessLogPath(), day1.AddDate(0, 0, 2), 30)
	if oldest.IsZero() || !oldest.Equal(day1) {
		t.Errorf("oldest = %v want %v", oldest, day1)
	}
	if s := stats["aa11bb22"]; s.Days != 3 {
		t.Errorf("decisionLog stat %+v", s)
	}
	if s := stats["cc33dd44"]; s.Days != 1 {
		t.Errorf("conventions stat %+v", s)
	}
}

func TestAccessNominationsHotAndCold(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for d := range 5 {
		logAccess(now.AddDate(0, 0, -d), "aa11bb22", accessRecall)
	}
	// Log must cover > coldDays for a cold nomination to fire at all.
	logAccess(now.AddDate(0, 0, -(coldDays+5)), "ee55ff66", accessRecall)

	noms := accessNominations(now)
	joined := strings.Join(noms, "\n")
	if !strings.Contains(joined, "home/aa11bb22: hot") {
		t.Errorf("want a hot nomination, got %v", noms)
	}
	if !strings.Contains(joined, "home/ee55ff66: not recalled") {
		t.Errorf("want a cold nomination, got %v", noms)
	}
}

// A repeat same-day inject adds no line; a new day, a new entry or an explicit search still does.
func TestLogAccessRecallInjectOncePerEntryPerDay(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	logAccessBatch(now, []string{"aa000001", "aa000001", "bb000002"}, accessRecall, "")
	logAccess(now.Add(time.Hour), "aa000001", accessRecall)
	logAccess(now.Add(time.Hour), "aa000001", AccessSearch)
	logAccess(now.AddDate(0, 0, 1), "aa000001", accessRecall)
	data, _ := os.ReadFile(accessLogPath())
	if got := strings.Count(string(data), "\n"); got != 4 {
		t.Errorf("want 4 lines (aa+bb injects day 1, aa search, aa inject day 2), got %d:\n%s", got, data)
	}
}

// A busy day of repeat injects no longer evicts history: hot fires after 3 days, cold after a 90d-old log.
func TestAccessNominationsSurviveBusyDays(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	logAccess(now.AddDate(0, 0, -(coldDays+5)), "ee55ff66", accessRecall)
	for d := range hotMinDays {
		for range 50 {
			logAccess(now.AddDate(0, 0, -d), "aa11bb22", accessRecall)
		}
	}
	joined := strings.Join(accessNominations(now), "\n")
	if !strings.Contains(joined, "home/aa11bb22: hot") || !strings.Contains(joined, "home/ee55ff66: not recalled") {
		t.Errorf("want hot and cold nominations, got %q", joined)
	}
}

// An unopenable lock file degrades to a plain append: no rotate runs unlocked.
func TestLogAccessLockUnopenableAppendsOnly(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	path := accessLogPath()
	logAccess(now.AddDate(0, 0, -(accessRotateDays+1)), "aa000001", accessRecall)
	_ = os.Remove(path + ".lock")
	if err := os.Mkdir(path+".lock", 0o700); err != nil { // a directory cannot be opened as the lock file
		t.Fatal(err)
	}
	logAccess(now, "bb000002", accessRecall)
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("rotation ran without the lock")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "aa000001") || !strings.Contains(string(data), "bb000002") {
		t.Errorf("fallback must append to the existing log, got %q", data)
	}
}

// Main-format lines parse as before, plain readers skip session records, and an old log still counts.
func TestAccessLineShapesAndOldLogCounted(t *testing.T) {
	tempConfig(t)
	const sid = "11111111-1111-1111-1111-111111111111"
	for _, c := range []struct{ line, entry, reason, session string }{
		{"2026-11-10T10:00:00Z -- aa000001 -- memory show", "aa000001", "memory show", ""},
		{"2026-11-10T10:00:00Z -- aa000001 -- odd -- tail", "aa000001", "odd -- tail", ""},
		{"2026-11-10T10:00:00Z -- aa000001", "aa000001", "", ""},
		{"session -- " + sid + " -- 2026-11-10T10:00:00Z -- aa000001 -- memory show", "aa000001", "memory show", sid},
		{"garbage", "", "", ""},
	} {
		ts, entry, reason, session := parseAccessLine(c.line)
		if entry != c.entry || reason != c.reason || session != c.session || (c.entry != "" && ts.IsZero()) {
			t.Errorf("%q = %v %q %q %q", c.line, ts, entry, reason, session)
		}
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	path := accessLogPath()
	write(t, path, "2026-11-10T10:00:00Z -- aa000001 -- prompt-recall inject\n2026-11-11T10:00:00Z -- aa000001 -- memory show\n")
	logAccessBatch(now, []string{"aa000001"}, AccessShow, sid)
	stats, _ := accessStatsAt(path, now, 30)
	if got := returnCounts(now)["aa000001"]; stats["aa000001"].Days != 3 || got != 2 {
		t.Errorf("days = %d, returns = %d, want 3 and 2 (the session record must not count twice)", stats["aa000001"].Days, got)
	}
}

// A session's repeat inject of one id writes one session record a day; the plain lines stay one per id and day.
func TestAccessSessionRecordOncePerSessionIDDay(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for range 5 {
		logAccessBatch(now, []string{"aa000001", "bb000002"}, accessRecall, "11111111-1111-1111-1111-111111111111")
	}
	logAccessBatch(now, []string{"aa000001"}, accessRecall, "22222222-2222-2222-2222-222222222222")
	data, _ := os.ReadFile(accessLogPath())
	if got, want := strings.Count(string(data), "\n"), 2+3; got != want {
		t.Errorf("lines = %d, want %d (2 plain + 3 session records):\n%s", got, want, data)
	}
}

// One dispatch's block may reach LogBriefRecall from both the hook and ask; it still credits each id once.
func TestLogBriefRecallTwiceCreditsOnce(t *testing.T) {
	tempConfig(t)
	const block = "\n\n<memory-recall>\n- aa000001 one\n- bb000002 two\n</memory-recall>"
	for range 2 {
		LogBriefRecall("11111111-1111-1111-1111-111111111111", block)
	}
	data, _ := os.ReadFile(accessLogPath())
	if got := strings.Count(string(data), "session -- "); got != 2 {
		t.Errorf("session records = %d, want 2:\n%s", got, data)
	}
}

func logAccess(now time.Time, entry, reason string) {
	logAccessBatch(now, []string{entry}, reason, "")
}

// Access traces always live in state, never in the store.
func TestAccessLogPathOutsideStore(t *testing.T) {
	tempConfig(t)
	if got, want := accessLogPath(), filepath.Join(AccessStateDir(), "home.log"); got != want {
		t.Fatalf("accessLogPath() = %s, want %s", got, want)
	}
	logAccess(time.Now(), "aa11bb22", "read")
	if _, err := os.Stat(filepath.Join(paths.MemoryStoreDir(), "access.log")); !os.IsNotExist(err) {
		t.Errorf("access.log written inside the store: %v", err)
	}
	if data, _ := os.ReadFile(accessLogPath()); !strings.Contains(string(data), "aa11bb22") {
		t.Errorf("state access log missing the entry: %q", data)
	}
}

func TestLogAccessRedactsCredentialShapedReason(t *testing.T) {
	tempConfig(t)
	logAccess(time.Now(), "aa11bb22", "leaked sk-abcdefghijklmnopqrstuvwx in reason")
	data := mustRead(t, accessLogPath())
	if strings.Contains(data, "sk-abcdefghijklmnopqrstuvwx") {
		t.Error("a credential-shaped reason must be redacted, not logged verbatim")
	}
	if !strings.Contains(data, "redacted") {
		t.Errorf("log %q", data)
	}
}

func TestLearnRecall(t *testing.T) {
	tempConfig(t)
	now := time.Now().UTC()
	const elsewhere = "22222222-2222-2222-2222-222222222222"
	write(t, Path("conventions.md"), `# Conventions

- Used <!-- id:aaaa0001 -->
- Unused <!-- id:aaaa0002 -->
`)
	sessions := make([]string, recallMinInjections)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("11111111-1111-1111-1111-%012d", i)
		logAccessBatch(now.Add(-time.Minute), []string{"aaaa0001", "aaaa0002"}, accessRecall, sessions[i])
	}
	logAccessBatch(now.Add(-time.Minute), []string{"aaaa0003"}, accessRecall, sessions[0])
	for _, s := range sessions {
		t.Setenv(paths.SessionIDEnv, s)
		showRecallFixture(t, "aaaa0001")
	}
	t.Setenv(paths.SessionIDEnv, elsewhere)
	showRecallFixture(t, "aaaa0002")
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"aaaa0001": 1.2, "aaaa0002": 0.8, "aaaa0003": 1} {
		if got := boostFactor(recallWeights(), id); got != want {
			t.Errorf("%s factor = %v, want %v", id, got, want)
		}
	}
	assertRecallNoRewrite(t, now)
}

func showRecallFixture(t *testing.T, id string) {
	t.Helper()
	if _, _, err := Show(id); err != nil {
		t.Fatal(err)
	}
}

func assertRecallNoRewrite(t *testing.T, now time.Time) {
	t.Helper()
	path := recallLearningPath()
	stamp := now.Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := LearnRecall(now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(stamp) {
		t.Fatal("unchanged access log caused the learner to rewrite state")
	}
}

func BenchmarkRecallRanking(b *testing.B) {
	b.Cleanup(paths.SetStampNowForTest(func() time.Time { return time.Now().Add(time.Hour) }))
	for _, present := range []bool{false, true} {
		b.Run(fmt.Sprintf("weights=%t", present), func(b *testing.B) {
			b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
			b.Setenv("HOME", b.TempDir())
			b.Setenv(UpdateChildEnv, "")
			var bank strings.Builder
			bank.WriteString("# Conventions\n\n")
			for i := range 5 {
				fmt.Fprintf(&bank, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
			}
			write(b, Path("conventions.md"), bank.String())
			logAccess(time.Now(), "00000001", AccessShow)
			if present {
				path := filepath.Join(paths.MemoryStateDir(), "recall-learning.json")
				write(b, path, `{"days":{"2026-11-18":{"00000001":{"injected":5,"used":5}}}}`)
			}
			recallPicks(context.Background(), "", recallTask, false, false)
			for b.Loop() {
				recallPicks(context.Background(), "", recallTask, false, false)
			}
		})
	}
}

type recallIndexFixture struct {
	root, source, archive string
	key                   uint64
}

func newRecallIndexFixture(t testing.TB) recallIndexFixture {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	f := recallIndexFixture{
		root:    root,
		source:  Path("decisionLog.md"),
		archive: paths.MemorySub("archive", "sessionHistory-old.md"),
		key:     encoderKey(),
	}
	files := map[string]string{
		f.source: "# Decisions\n\n## router retry\nrouter retry latency\n" +
			"```\n## fenced marker\n- fenced marker\n```\n\n## the and of\nwith the and\n",
		Path("conventions.md"): "# Conventions\n\n- commit small\nreview focused changes\n",
		filepath.Join(paths.MemoryStoreDir(), "conventions.md"): "# Global\n\n- router retry\nbounded latency\n",
		f.archive: "# Archive\n\n## archive storm\nrouter retry during outage\n",
		Path("recall.md"): "---\nname: native recall\n" +
			"description: router context\n---\nNative recall preserves retry details.\n",
		Path("bank-topic.md"): "---\nname: excludedbanktoken\n" +
			"description: excludedbanktoken\nharness_kind: bank\n---\nexcludedbanktoken\n",
		filepath.Join(paths.MemorySubDir("knowledge"), "invariant.md"): "---\nname: strippedmetadata\n---\n" +
			"# knowledge invariant\nRouter retry requires bounded latency.\n",
	}
	for path, body := range files {
		recallIndexWrite(t, path, body, time.Now().Add(-time.Hour))
	}
	return f
}

func recallIndexWrite(t testing.TB, path, body string, mtime time.Time) {
	t.Helper()
	write(t, path, body)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// readIndex decodes the index mapped and releases the mapping when the test ends.
func readIndex(t testing.TB, path string, key uint64, want map[fileKey]bool) map[fileKey]indexed {
	t.Helper()
	m, release := readRecallIndex(path, key, want)
	t.Cleanup(release)
	return m
}

func (f recallIndexFixture) search(t testing.TB, query string) []SearchHit {
	t.Helper()
	return searchBanks(query, 0)
}

func (f recallIndexFixture) index(t testing.TB) map[fileKey]indexed {
	t.Helper()
	index := readIndex(t, filepath.Join(paths.MemoryStateDir(), recallIndexFile), f.key, nil)
	if len(index) == 0 {
		t.Fatal("no persisted index entries for the current encoder key")
	}
	return index
}

func (f recallIndexFixture) persisted(t testing.TB) map[fileKey]indexed {
	t.Helper()
	index := f.index(t)
	for _, file := range corpusOf() {
		assertPersistedRecallFile(t, file, index)
	}
	return index
}

func assertPersistedRecallFile(t testing.TB, file corpusFile, index map[fileKey]indexed) {
	t.Helper()
	info, err := os.Stat(file.Path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := index[fileKey{file.Path, file.Mode}]
	if !ok || entry.size != info.Size() || entry.mtimeNs != info.ModTime().UnixNano() {
		t.Fatalf("missing or stale persisted entry for %s (mode %d)", file.Path, file.Mode)
	}
	if !entry.seg.Valid() {
		t.Fatalf("invalid persisted segment for %s", file.Path)
	}
}

func TestRecallIndexWarmEqualsCold(t *testing.T) {
	f := newRecallIndexFixture(t)
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	queries := []string{"router", "router router retry", "commit small", "fenced marker",
		"native recall", "knowledge invariant", "archive storm", "the and of", "excludedbanktoken"}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			cold := f.search(t, query)
			f.persisted(t)
			warm := f.search(t, query)
			if !reflect.DeepEqual(warm, cold) {
				t.Fatalf("warm results differ from cold:\nwarm: %#v\ncold: %#v", warm, cold)
			}
		})
	}
}

func TestRecallIndexStatInvalidation(t *testing.T) {
	f := newRecallIndexFixture(t)
	before := f.search(t, "router galaxy retry")
	index := f.persisted(t)
	key := fileKey{f.source, segEntries}
	old := index[key]
	data, err := os.ReadFile(f.source)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(data), "router", "galaxy")
	if changed == string(data) || len(changed) != len(data) {
		t.Fatal("fixture edit must change content without changing size")
	}
	recallIndexWrite(t, f.source, changed, time.Unix(0, old.mtimeNs).Add(time.Minute))
	info, err := os.Stat(f.source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != old.size || info.ModTime().UnixNano() == old.mtimeNs {
		t.Fatal("fixture edit did not preserve size and change mtime")
	}
	after := f.search(t, "router galaxy retry")
	if reflect.DeepEqual(before, after) {
		t.Fatal("same-size source edit did not change search results")
	}
	updated := f.persisted(t)[key]
	if reflect.DeepEqual(updated.seg, old.seg) {
		t.Fatal("same-size source edit did not replace the persisted segment")
	}
}

func TestRecallIndexRacyNotPersisted(t *testing.T) {
	f := newRecallIndexFixture(t)
	now := time.Now()
	if err := os.Chtimes(f.source, now, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		hits := f.search(t, "fenced marker")
		if len(hits) == 0 {
			t.Fatal("racy source was not searched")
		}
		index := f.index(t)
		if _, ok := index[fileKey{f.source, segEntries}]; ok {
			t.Fatal("racy source segment was persisted")
		}
		if _, ok := index[fileKey{f.archive, segEntries}]; !ok {
			t.Fatal("backdated archive segment was not persisted")
		}
	}
}

func TestRecallIndexKeyMismatchIgnored(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	index := f.persisted(t)
	for key, entry := range index {
		entry.seg = encodeSegment([]string{"## poisoned router\nrouter router retry"})
		index[key] = entry
	}
	wrong := f.key + 1
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	if err := writeRecallIndex(path, wrong, index); err != nil {
		t.Fatal(err)
	}
	if len(readIndex(t, path, wrong, nil)) != len(index) {
		t.Fatal("fixture did not persist the mismatched encoder key")
	}
	if len(readIndex(t, path, f.key, nil)) != 0 {
		t.Fatal("index with mismatched encoder key was accepted")
	}
	f.search(t, "router retry")
	f.persisted(t)
	if len(readIndex(t, path, wrong, nil)) != 0 {
		t.Fatal("rewritten index still carries the wrong encoder key")
	}
}

func TestRecallIndexCorruptFailsOpen(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	f.persisted(t)
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"truncated-header", data[:12]},
		{"truncated-entry", data[:len(data)-1]},
		{"garbage", []byte(strings.Repeat("not a recall index", 8))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write(t, path, string(tt.data))
			if len(readIndex(t, path, f.key, nil)) != 0 {
				t.Fatal("corrupt index was accepted")
			}
			f.search(t, "router retry")
			f.persisted(t)
		})
	}

	// A corrupt wanted entry is a miss and a corrupt unrelated entry is never examined; the rest still load.
	t.Run("corrupt-entry", func(t *testing.T) {
		index := f.persisted(t)
		bad, good := fileKey{f.source, segEntries}, fileKey{f.archive, segEntries}
		index[bad] = indexed{index[bad].size, index[bad].mtimeNs, segView("corrupt segment")}
		if err := writeRecallIndex(path, f.key, index); err != nil {
			t.Fatal(err)
		}
		if _, ok := readIndex(t, path, f.key, map[fileKey]bool{bad: true, good: true})[bad]; ok {
			t.Fatal("corrupt wanted entry was accepted")
		}
		got := readIndex(t, path, f.key, map[fileKey]bool{good: true})
		if len(got) != 1 || !got[good].seg.Valid() {
			t.Fatalf("unrelated corrupt entry changed the wanted read: %d entries", len(got))
		}
		f.search(t, "router retry")
		f.persisted(t)
	})
}

// A torn write (right length, one byte wrong inside a segment) must miss, not feed wrong text to recall.
func TestRecallIndexTornWriteIsAMiss(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	f.persisted(t)
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	write(t, path, string(data))
	if len(readIndex(t, path, f.key, nil)) != 0 {
		t.Fatal("index with a flipped byte and a stale CRC was accepted")
	}
}

func TestRecallIndexPrunesMissing(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "archive storm")
	index := f.persisted(t)
	key := fileKey{f.archive, segEntries}
	if _, ok := index[key]; !ok {
		t.Fatal("archive segment was not persisted before deletion")
	}
	if err := os.Remove(f.archive); err != nil {
		t.Fatal(err)
	}
	// A changed surviving source triggers the write that prunes deleted entries.
	recallIndexWrite(t, f.source, "# Decisions\n\n## router retry\nupdated router policy\n",
		time.Now().Add(-time.Hour))
	if hits := f.search(t, "archive storm"); len(hits) != 0 {
		t.Fatalf("deleted archive still returned hits: %#v", hits)
	}
	f.search(t, "updated router policy")
	if _, ok := f.persisted(t)[key]; ok {
		t.Fatal("deleted archive segment was not pruned")
	}
}

func BenchmarkSearchBanksTermsWarm(b *testing.B) {
	f := newRecallIndexFixture(b)
	const query = "router router retry native recall archive storm"
	f.search(b, query)
	f.persisted(b)
	terms := contentTerms(query)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		searchRecallTerms(terms, 0, true)
	}
}

func BenchmarkReadRecallIndex30MB(b *testing.B) {
	root := b.TempDir()
	b.Setenv("CLAUDE_CONFIG_DIR", root)
	path, key, want, infoSize := write30MBRecallIndexFixture(b, root)
	if got := readIndex(b, path, key, nil); len(got) != want {
		b.Fatalf("fixture read returned %d entries, want %d", len(got), want)
	}
	b.SetBytes(infoSize)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, release := readRecallIndex(path, key, nil)
		if len(got) != want {
			b.Fatalf("read returned %d entries, want %d", len(got), want)
		}
		release()
	}
	b.ReportMetric(float64(infoSize), "index-bytes")
	b.ReportMetric(float64(want), "entries")
}

func write30MBRecallIndexFixture(b *testing.B, root string) (string, uint64, int, int64) {
	b.Helper()
	blocks := make([]string, 256)
	for i := range blocks {
		blocks[i] = fmt.Sprintf("## Router retry decision %d <!-- id:%08x -->\n%s", i, i+1,
			strings.Repeat("Router retry policy keeps deployment latency bounded while agents review the build gate. ", 3))
	}
	seg := encodeSegment(blocks)
	if !seg.Valid() {
		b.Fatal("invalid fixture segment")
	}
	index := make(map[fileKey]indexed)
	sourceSize := int64(len(strings.Join(blocks, "\n\n")))
	size := indexHeaderLen + indexTrailerLen
	for i := 0; size < 30_000_000; i++ {
		fk := fileKey{filepath.Join(root, "archive", fmt.Sprintf("sessionHistory-%04d.md", i)), segEntries}
		index[fk] = indexed{size: sourceSize, mtimeNs: 1, seg: seg}
		size += 25 + len(fk.path) + len(seg)
	}
	path := filepath.Join(root, recallIndexFile)
	key := encoderKey()
	if err := writeRecallIndex(path, key, index); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(size) || size > 31_000_000 {
		b.Fatalf("unexpected fixture stat: err=%v size=%d", err, size)
	}
	return path, key, len(index), info.Size()
}

// Segments stay valid and unchanged after the index file is replaced, the only way a writer changes it.
func TestRecallIndexMappedSurvivesRename(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	m, release := readRecallIndex(path, f.key, nil)
	defer release()
	before := map[fileKey]string{}
	for k, e := range m {
		before[k] = string(e.seg)
	}
	if len(before) == 0 {
		t.Fatal("no entries mapped")
	}
	if err := writeRecallIndex(path, f.key, map[fileKey]indexed{}); err != nil {
		t.Fatal(err)
	}
	for k, e := range m {
		if !e.seg.Valid() || string(e.seg) != before[k] {
			t.Fatalf("segment for %s changed after the index was replaced", k.path)
		}
	}
	if len(readIndex(t, path, f.key, nil)) != 0 {
		t.Fatal("replaced index was not read")
	}
}

// The process-retained mapping follows the file: a rewritten index is re-mapped, never served stale.
func TestRecallIndexRetainedFollowsRewrite(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	path := filepath.Join(paths.MemoryStateDir(), recallIndexFile)
	first := retainedIndexData(path)
	if len(first) == 0 || &retainedIndexData(path)[0] != &first[0] {
		t.Fatal("unchanged index was not served from the retained mapping")
	}
	if err := writeRecallIndex(path, f.key, map[fileKey]indexed{}); err != nil {
		t.Fatal(err)
	}
	if len(decodeRecallIndex(retainedIndexData(path), f.key, nil)) != 0 {
		t.Fatal("retained mapping served the replaced index")
	}
	if !decodeRecallIndex(first, f.key, nil)[fileKey{f.source, segEntries}].seg.Valid() {
		t.Fatal("old mapping was invalidated by the rewrite")
	}
}
