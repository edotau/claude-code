package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// TestRememberUnknownTypeErrors: an unmapped --type is refused, naming the valid set.
func TestRememberUnknownTypeErrors(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("bogus", "a fact", "", time.Now()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Remember with an unknown type = %v, want an error naming the valid types", err)
	}
}

// TestRememberEmptyFactErrors: whitespace-only input is refused before any write.
func TestRememberEmptyFactErrors(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("feedback", "   ", "", time.Now()); err == nil {
		t.Fatal("Remember with an empty fact = nil error, want one")
	}
}

func TestRememberProjectTypeWritesDatedDecisionLogEntryAtTop(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	reads := countBankReads(t)

	id, err := RememberSupersedes("project", "adopt bbolt for the memory store", "", now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned an empty id")
	}
	path := Path("decisionLog.md")
	if n := reads[path]; n != 2 {
		t.Errorf("Remember read decisionLog.md %d times, want 2 (its write, then settle)", n)
	}
	body := mustRead(t, path)
	if !strings.Contains(body, "## 2026-10-23 — adopt bbolt for the memory store <!-- id:"+id+" -->") {
		t.Errorf("decisionLog.md lacks the dated heading stamped with returned id %q: %q", id, body)
	}
	assertLaterProjectEntryFirst(t, id, now)
}

func assertLaterProjectEntryFirst(t *testing.T, id string, now time.Time) {
	t.Helper()
	// Round-trip: a second, later entry must still land ABOVE the first (newest-first).
	id2, err := RememberSupersedes("project", "a second, later decision", "", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(Path("decisionLog.md"))
	first := strings.Index(string(data), id2)
	second := strings.Index(string(data), id)
	if first == -1 || second == -1 || first > second {
		t.Errorf("newer entry (%s) did not land above the older one (%s): %q", id2, id, data)
	}
}

func TestRememberNonProjectTypesWriteConventionsBullet(t *testing.T) {
	tempConfig(t)
	for _, kind := range []string{"user", "feedback", "reference"} {
		t.Run(kind, func(t *testing.T) {
			id, err := RememberSupersedes(kind, "prefer early returns over deep nesting", "", time.Now())
			if err != nil {
				t.Fatalf("Remember(%s): %v", kind, err)
			}
			label := strings.ToUpper(kind[:1]) + kind[1:]
			body := mustRead(t, Path("conventions.md"))
			if !strings.Contains(body, "- "+label+": prefer early returns over deep nesting") {
				t.Errorf("conventions.md missing the labeled bullet: %q", body)
			}
			if !strings.Contains(body, "<!-- id:"+id+" -->") {
				t.Errorf("conventions.md missing the returned id %s: %q", id, body)
			}
		})
	}
}

// TestPrependBlockUnreadableLogIsNotRewritten: a read error other than not-exist leaves decisionLog.md alone.
func TestPrependBlockUnreadableLogIsNotRewritten(t *testing.T) {
	runBank(t, func(t *testing.T) {
		p := Path("decisionLog.md") // a directory: ReadFile fails with EISDIR
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := writeRememberFact("decisionLog.md", "project", "x", "", time.Now()); err == nil {
			t.Fatal("writeRememberFact on an unreadable log = nil error, want the read error")
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Fatalf("unreadable log was replaced: %v", err)
		}
	})
}

func TestLastStampedIDExactMatchAndDirection(t *testing.T) {
	t.Parallel()
	p := "# c\n\n- User: foo bar <!-- id:aaaa1111 -->\n- User: foo <!-- id:bbbb2222 -->\n- User: foo <!-- id:cccc3333 -->\n"
	if got, err := lastStampedID(p, "- User: foo", false); err != nil || got != "cccc3333" {
		t.Fatalf("appended file: got %q, %v, want cccc3333", got, err)
	}
	if got, err := lastStampedID(p, "- User: foo", true); err != nil || got != "bbbb2222" {
		t.Fatalf("newest-first file: got %q, %v, want bbbb2222", got, err)
	}
	if got, err := lastStampedID(p, "- User: foo bar\ncontinued", false); err != nil || got != "aaaa1111" {
		t.Fatalf("multi-line fact: got %q, %v, want aaaa1111", got, err)
	}
}

// A fact into an absent conventions.md lands under the scaffold header, not appendBlocks' "(archived)" one.
func TestRememberSeedsMissingFileFromScaffold(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("user", "likes tea", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := mustRead(t, Path("conventions.md"))
	scaffold := strings.TrimRight(templates()["conventions.md"], "\n")
	if strings.Contains(body, "(archived)") || !strings.HasPrefix(body, scaffold) {
		t.Errorf("conventions.md = %q, want it to start with the scaffold %q", body, scaffold)
	}
}

// TestRememberRefusesFactItCannotStamp: a fact that would get no id of its own errors and leaves the file untouched.
func TestRememberRefusesFactItCannotStamp(t *testing.T) {
	tempConfig(t)
	path := Path("conventions.md")
	for _, c := range []struct{ name, body, fact string }{
		{"unclosed fence above", "# Conventions\n\n- a rule\n\n```go\nx := 1\n", "fact after the fence"},
		{"id marker in the fact", "# Conventions\n\n- a rule\n", "quote <!-- id:deadbeef --> here"},
	} {
		write(t, path, c.body)
		if _, err := RememberSupersedes("feedback", c.fact, "", time.Now()); err == nil {
			t.Errorf("%s: Remember = nil error, want a refusal", c.name)
		}
		if got := mustRead(t, path); got != c.body {
			t.Errorf("%s: conventions.md rewritten despite the error:\n%s", c.name, got)
		}
	}
}

// TestRememberSettlesInsideItsWriteLock: no second writer can take the bank lock between the fact write and the settle.
func TestRememberSettlesInsideItsWriteLock(t *testing.T) {
	tempConfig(t)
	origWait, origHook := BankLockWait, afterRememberWrite
	t.Cleanup(func() { BankLockWait, afterRememberWrite = origWait, origHook })
	BankLockWait = 200 * time.Millisecond
	release := make(chan struct{})
	intruded := false
	afterRememberWrite = func() { // a rival writer that grabs the lock if it is free and holds it past BankLockWait
		held, refused := make(chan struct{}), make(chan struct{})
		go func() {
			hold := func() error { close(held); <-release; return nil }
			if ran, _ := paths.TryFileLock(BankLockPath(), time.Millisecond, hold); !ran {
				close(refused)
			}
		}()
		select {
		case <-held:
			intruded = true
		case <-refused:
		}
	}
	_, err := RememberSupersedes("feedback", "settle in the same critical section", "", time.Now())
	close(release)
	if intruded || err != nil {
		t.Fatalf("Remember released the bank lock before settling: rival took it = %v, Remember err = %v", intruded, err)
	}
}

// lockStoreIndex returns a hook that plants .git/index.lock in a git-backed store, so settle's commit fails.
func lockStoreIndex(t *testing.T) func() {
	t.Helper()
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(paths.MemoryStoreDir(), ".git", "index.lock")
	t.Cleanup(func() { _ = os.Remove(lock) })
	return func() {
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Error(err)
		}
	}
}

// TestRememberSettleFailureRollsBackTheWrite: a failed settle leaves the bank file as it was; a retry is no duplicate.
func TestRememberSettleFailureRollsBackTheWrite(t *testing.T) {
	tempConfig(t)
	repoRoot(t, "proj")
	seedConventionBlock(t)
	path := Path("conventions.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := afterRememberWrite
	t.Cleanup(func() { afterRememberWrite = orig })
	afterRememberWrite = lockStoreIndex(t)
	const fact = "retry after a failed settle"
	if _, err := RememberSupersedes("feedback", fact, "", time.Now()); err == nil || !strings.Contains(err.Error(), "settle: ") {
		t.Fatalf("Remember with a failing settle = %v, want an error naming the settle", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("failed Remember left the bank changed:\nbefore %q\nafter  %q", before, after)
	}
	afterRememberWrite = orig
	_ = os.Remove(filepath.Join(paths.MemoryStoreDir(), ".git", "index.lock"))
	if _, err := RememberSupersedes("feedback", fact, "", time.Now()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if after, _ := os.ReadFile(path); strings.Count(string(after), fact) != 1 {
		t.Fatalf("retry left the fact %d times, want 1: %q", strings.Count(string(after), fact), after)
	}
}

func TestAppendReportRollsBackWhenDecisionsFail(t *testing.T) {
	tempConfig(t)
	previousNow := now
	now = func() time.Time { return time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = previousNow })
	for _, previous := range []string{"", "# Existing\n\nKeep these bytes.\t"} {
		t.Run(previous, func(t *testing.T) {
			runBank(t, func(t *testing.T) {
				report := paths.MemorySub("reports", "2026-11-08-review.md")
				decisions := paths.MemorySub("reports", "2026-11-08-decisions.md")
				if previous != "" {
					write(t, report, previous)
					report = paths.MemorySub("reports", "2026-11-08-review-2.md") // a taken name gets a suffix
				}
				if err := os.MkdirAll(decisions, 0o700); err != nil {
					t.Fatal(err)
				}
				in := ReportInput{Report: "# Review\n\nUnique report body.", Decisions: []string{"Keep it."}}
				got, err := appendReport("review", in)
				if err == nil || len(got) != 0 {
					t.Fatalf("failed decisions write = %v, %v; want no paths and an error", got, err)
				}
				assertReportRolledBack(t, report, "")
				if err := os.Remove(decisions); err != nil {
					t.Fatal(err)
				}
				if _, err := appendReport("review", in); err != nil {
					t.Fatalf("retry report: %v", err)
				}
				assertReportTextOccursOnce(t, report, "Unique report body.")
				assertReportTextOccursOnce(t, decisions, "Keep it.")
				if previous != "" {
					assertReportRolledBack(t, paths.MemorySub("reports", "2026-11-08-review.md"), previous)
				}
			})
		})
	}
}

func assertReportTextOccursOnce(t *testing.T, path, text string) {
	t.Helper()
	if got := strings.Count(mustRead(t, path), text); got != 1 {
		t.Fatalf("retry saved %q in %s %d times, want 1", text, path, got)
	}
}

func assertReportRolledBack(t *testing.T, path, previous string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if previous == "" {
		if !os.IsNotExist(err) {
			t.Fatalf("new report survived failed decisions write: %q, %v", got, err)
		}
		return
	}
	if err != nil || string(got) != previous {
		t.Fatalf("existing report not restored byte-for-byte: got %q, %v; want %q", got, err, previous)
	}
}

func TestDecisionLinesPreservesLiteralWhitespace(t *testing.T) {
	got := decisionLines("2026-11-08", "review", []string{
		"\t Keep `a  b\tc` and \"x  y\tz\"\r\nnext\nlast\rtail \t",
		" \t\r\n ",
	})
	want := "- 2026-11-08 · review · Keep `a  b\tc` and \"x  y\tz\" next last tail\n"
	if got != want {
		t.Fatalf("decision whitespace changed: got %q, want %q", got, want)
	}
}

func TestAppendTextSeparatesUnterminatedMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, previous, text, want string }{
		{"heading", "# Existing", "# New\n", "# Existing\n# New\n"},
		{"bullet", "- Existing", "- New\n", "- Existing\n- New\n"},
		{"terminated", "# Existing\n", "# New\n", "# Existing\n# New\n"},
		{"empty", "", "# New\n", "# New\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.md")
			write(t, path, tc.previous)
			if err := appendText(path, "# Unused header\n\n", tc.text); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, path); got != tc.want {
				t.Fatalf("append merged Markdown or added a blank line: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAppendReportRejectsDecisionsLabel(t *testing.T) {
	tempConfig(t)
	for _, label := range []string{"decisions", "DECISIONS", "Decisions"} {
		t.Run(label, func(t *testing.T) {
			got, err := appendReport(label, ReportInput{
				Report:    "# Review\n\nReport body.\n",
				Decisions: []string{"Keep reports separate from decisions."},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("appendReport(%q) = %v, %v; want reserved-label error", label, got, err)
			}
			if len(got) != 0 {
				t.Fatalf("rejected report returned paths: %v", got)
			}
			if _, err := os.Stat(paths.MemorySubDir("reports")); !os.IsNotExist(err) {
				t.Fatalf("rejected label created reports directory: %v", err)
			}
		})
	}
}

// A failed store commit rolls back both appends, so a retry never duplicates the report or its decisions.

// not a repo: commitStore fails

func TestRememberWritesTheBanksFileInTheBank(t *testing.T) {
	runRoot(t, func(t *testing.T, root string) {
		if _, err := Init(); err != nil {
			t.Fatal(err)
		}
		id, err := RememberSupersedes("user", "prefer small diffs", "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		body := mustRead(t, Path("conventions.md"))
		if !strings.Contains(body, "prefer small diffs") || !strings.Contains(body, "id:"+id) {
			t.Errorf("conventions.md missing the fact or its id %s: %q", id, body)
		}
	})
}

// TestRememberSupersedesStrikesOldEntry: --supersedes writes the edge and settle strikes the old entry.
func TestRememberSupersedesStrikesOldEntry(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	oldID, err := RememberSupersedes("project", "first plan", "", now)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := RememberSupersedes("project", "second plan", oldID, now)
	if err != nil {
		t.Fatal(err)
	}
	body := mustRead(t, Path("decisionLog.md"))
	for _, want := range []string{"~~first plan~~", "Replaced by " + newID, "Supersedes " + oldID + "."} {
		if !strings.Contains(body, want) {
			t.Errorf("decisionLog.md lacks %q: %q", want, body)
		}
	}
}

// TestRememberSupersedesRefusals: an unknown or already-replaced id is refused and the bank is left untouched.
func TestRememberSupersedesRefusals(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("project", "x", "deadbeef", time.Now()); err == nil {
		t.Fatal("RememberSupersedes with an unknown id = nil error, want one")
	}
	if _, err := os.Stat(paths.MemoryStoreDir()); err == nil {
		t.Error("a refused supersede created the store")
	}
	a, _ := RememberSupersedes("project", "A", "", time.Now())
	b, err := RememberSupersedes("project", "B", a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, Path("decisionLog.md"))
	_, err = RememberSupersedes("project", "C", a, time.Now())
	if err == nil || !strings.Contains(err.Error(), "already replaced by "+b) {
		t.Fatalf("superseding a replaced entry = %v, want a refusal naming %s", err, b)
	}
	if mustRead(t, Path("decisionLog.md")) != before {
		t.Error("a refused supersede changed decisionLog.md")
	}
}

// TestRememberFeedbackClassWritesNote: a feedback-<class> type writes a named, front-mattered note and indexes it.
func TestRememberFeedbackClassWritesNote(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	rel, err := RememberSupersedes("feedback-correction", "Never claim a test passed without running it: ran it", "", now)
	if err != nil || rel != "feedback/feedback-never-claim-a-test-passed-without-running-it-ran-it.md" {
		t.Fatalf("Remember = %q, %v; want the feedback/ note path", rel, err)
	}
	note := mustRead(t, filepath.Join(paths.MemoryStoreDir(), rel))
	wants := []string{"name: feedback-never-claim", "  type: feedback\n  class: correction\n", "\n\nNever claim a test"}
	for _, want := range wants {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %q", want, note)
		}
	}
	if idx := mustRead(t, catalogPath()); !strings.Contains(idx, "("+rel+")") {
		t.Errorf("index.md has no line for %s: %q", rel, idx)
	}
	again, err := RememberSupersedes("feedback-correction", "Never claim a test passed without running it: ran it", "", now)
	if err != nil || again == rel {
		t.Fatalf("a repeat = %q, %v; want a distinct note, never an overwrite", again, err)
	}
	if _, err := RememberSupersedes("feedback-process", "x", "abcd1234", now); err == nil {
		t.Error("--supersedes on a note type = nil error, want one")
	}
}

// TestDropCommitsSinceMovesHeadBack: a failed write leaves no commit past the pre-write HEAD.
func TestDropCommitsSinceMovesHeadBack(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("project", "first", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := StoreRepo().headSHA()
	if _, err := RememberSupersedes("project", "second", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if head, _ := StoreRepo().headSHA(); head == before {
		t.Fatal("settle made no commit to drop") // the assertion below would be vacuous
	}
	if err := dropCommitsSince(before, "settle: "+stateKey(), errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if head, _ := StoreRepo().headSHA(); head != before {
		t.Errorf("HEAD = %s, want the pre-write %s", head, before)
	}
}

// TestFeedbackNoteSettleFailureKeepsTheNote: a failed settle never deletes the note a retry or a peer may rely on.
func TestFeedbackNoteSettleFailureKeepsTheNote(t *testing.T) {
	tempConfig(t)
	lockStoreIndex(t)()
	rel, err := RememberSupersedes("feedback-process", "keep me after settle fails", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("remember with a failing settle = %v, want an error saying the note was kept", err)
	}
	if !statOK(filepath.Join(paths.MemoryStoreDir(), rel)) {
		t.Fatalf("note %s was removed by the failed settle", rel)
	}
}

// TestDropCommitsSinceKeepsPeerCommits: two commits past the pre-write HEAD means a peer's is among them.
func TestDropCommitsSinceKeepsPeerCommits(t *testing.T) {
	tempConfig(t)
	if _, err := RememberSupersedes("project", "first", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := StoreRepo().headSHA()
	for _, f := range []string{"second", "third"} {
		if _, err := RememberSupersedes("project", f, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	head, _ := StoreRepo().headSHA()
	if err := dropCommitsSince(before, "settle: "+stateKey(), errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if after, _ := StoreRepo().headSHA(); after != head {
		t.Errorf("HEAD moved to %s, want %s kept", after, head)
	}
}

func TestJobReportNamesAreUnique(t *testing.T) {
	runBank(t, func(t *testing.T) {
		day := time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC)
		a, err := JobReport(day, "review", "my task/x", []byte("body"))
		b, err2 := JobReport(day, "review", "my task/x", []byte("body"))
		if err != nil || err2 != nil || a == b || filepath.Base(a) != "2026-11-08-review-my-task-x.md" ||
			filepath.Base(b) != "2026-11-08-review-my-task-x-2.md" {
			t.Fatalf("paths %q %q, errors %v %v", a, b, err, err2)
		}
	})
}

func TestJobReportHostileLabel(t *testing.T) {
	runBank(t, func(t *testing.T) {
		day := time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC)
		for _, label := range []string{"../../etc/passwd", "-rf\n\x00", "..", strings.Repeat("a", 500)} {
			p, err := JobReport(day, "audit", label, []byte("b"))
			base := filepath.Base(p)
			if err != nil || filepath.Dir(p) != paths.MemorySubDir("reports") || len(base) > 100 {
				t.Fatalf("label %q: path %q, %v", label, p, err)
			}
			if strings.ContainsAny(base, "/\x00\n") || strings.HasPrefix(base, "-") {
				t.Fatalf("label %q: unsafe name %q", label, base)
			}
		}
	})
}

// TestRememberSameFactSameClockGetsDistinctIDs: a repeat prepended at the same index and clock never reuses an id.
func TestRememberSameFactSameClockGetsDistinctIDs(t *testing.T) {
	tempConfig(t)
	now := time.Date(2026, 11, 10, 1, 2, 3, 0, time.UTC)
	first, err := RememberSupersedes("project", "same fact", "", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RememberSupersedes("project", "same fact", "", now)
	if err != nil || second == first {
		t.Fatalf("second Remember = %q, %v; want an id distinct from the first, %q", second, err, first)
	}
	if body := mustRead(t, Path("decisionLog.md")); strings.Count(body, "<!-- id:"+second+" -->") != 1 {
		t.Errorf("id %s is not unique in decisionLog.md:\n%s", second, body)
	}
}

// Remember writes only its new entry, lint-clean with a readable id; unformatted entries around it keep their bytes.
func TestRememberWritesOnlyTheNewEntry(t *testing.T) {
	legacy := strings.Repeat("legacy  text ", 14)
	for _, c := range []struct{ kind, file, before, fact string }{
		{"user", "conventions.md", "# Conventions\n\n>  odd   header\n\n- User:   odd " + legacy + "<!-- id:aaaa1111 -->\n",
			strings.Repeat("fresh words ", 15)},
		{"project", "decisionLog.md", "# Decision Log\n\n>  odd   header\n\n## 2026-01-01 — old <!-- id:aaaa1111 -->\n" + legacy + "\n",
			"fresh decision"},
	} {
		t.Run(c.file, func(t *testing.T) {
			tempConfig(t)
			write(t, Path(c.file), c.before)
			id, err := RememberSupersedes(c.kind, c.fact, "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			after := mustRead(t, Path(c.file))
			at := len(c.before) // conventions appends; decisionLog inserts above its first entry
			if c.file == "decisionLog.md" {
				at = strings.Index(c.before, "## ")
			}
			head, tail := c.before[:at], c.before[at:]
			if !strings.HasPrefix(after, head) || !strings.HasSuffix(after, tail) {
				t.Fatalf("untouched bytes changed:\nbefore %q\nafter  %q", c.before, after)
			}
			added := strings.TrimSpace(after[len(head) : len(after)-len(tail)])
			assertLintClean(t, added)
			if e := ParseDoc(c.file, []byte(added)).Entries; len(e) != 1 || e[0].ID != id {
				t.Errorf("new entry does not read back with id %s: %q", id, added)
			}
		})
	}
}

// Settle after Remember stamps an unstamped entry of another id file; that file's other entries keep their bytes.
func TestRememberStampsOtherIDFiles(t *testing.T) {
	tempConfig(t)
	long := "- User:   odd " + strings.Repeat("legacy  text ", 14) + "<!-- id:aaaa1111 -->\n"
	write(t, Path("conventions.md"), "# Conventions\n\n"+long+"\n- unstamped rule\n")
	if _, err := RememberSupersedes("project", "a decision", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, Path("conventions.md"))
	if !strings.HasPrefix(after, "# Conventions\n\n"+long+"\n- unstamped rule <!-- id:") {
		t.Fatalf("settle changed an untouched entry or left the new one unstamped:\n%s", after)
	}
	if e := ParseDoc("conventions.md", []byte(after)).Entries; len(e) != 2 || e[1].ID == "" {
		t.Errorf("the stamped entry has no readable id: %+v", e)
	}
}

// The entry goes on top; a fence left open below does not block its id.
func TestRememberStampsDecisionAboveUnclosedFenceBelow(t *testing.T) {
	tempConfig(t)
	path := Path("decisionLog.md")
	write(t, path, templates()["decisionLog.md"]+"\n## 2026-01-01 — old\n\n```go\nx := 1\n")
	id, err := RememberSupersedes("project", "new fact", "", time.Now())
	if err != nil || id == "" {
		t.Fatalf("Remember = %q, %v; want an id", id, err)
	}
	if got := mustRead(t, path); !strings.Contains(got, id) || !strings.HasSuffix(got, "```go\nx := 1\n") {
		t.Errorf("decisionLog.md = %q", got)
	}
}

func TestRememberRefusesDuplicateInsideFence(t *testing.T) {
	tempConfig(t)
	before := "# C\n\n- User: repeat <!-- id:12345678 -->\n\n```\n"
	write(t, Path("conventions.md"), before)
	if id, err := writeRememberFact("conventions.md", "user", "repeat", "", testEnv().now); err == nil {
		t.Fatalf("reported old id %s as a new write inside a fence", id)
	}
	if got := mustRead(t, Path("conventions.md")); got != before {
		t.Errorf("refused write changed file: %q", got)
	}
}

func TestRememberKeepsOutsideEdit(t *testing.T) {
	tempConfig(t)
	path := Path("conventions.md")
	write(t, path, "# C\n\n- before <!-- id:12345678 -->\n")
	outside := "# C\n\n- outside <!-- id:23456789 -->\n"
	prev := readFile
	t.Cleanup(func() { readFile = prev })
	readFile = func(p string) ([]byte, error) {
		b, err := os.ReadFile(p)
		if p == path && err == nil {
			write(t, path, outside)
		}
		return b, err
	}
	if _, err := writeRememberFact("conventions.md", "user", "new", "", testEnv().now); err == nil {
		t.Fatal("outside edit reported as a successful write")
	}
	if got := mustRead(t, path); got != outside {
		t.Fatalf("outside edit lost: %q", got)
	}
}

func TestRememberPreservesInlineIDExample(t *testing.T) {
	for _, kind := range []string{"user", "project"} {
		t.Run(kind, func(t *testing.T) {
			tempConfig(t)
			file := RememberTypes[kind]
			before := "# Bank\n\n"
			write(t, Path(file), before)
			fact := "use ``literal ` <!-- id:11111111 --> code`` as an example"
			id, err := RememberSupersedes(kind, fact, "", testEnv().now)
			if err != nil || id == "" || id == "11111111" {
				t.Fatalf("Remember = %q, %v", id, err)
			}
			got := mustRead(t, Path(file))
			d := ParseDoc(file, []byte(got))
			if !strings.HasPrefix(got, before) || len(d.Entries) != 1 || d.Entries[0].ID != id || !strings.Contains(got, fact) {
				t.Errorf("lost original bytes, id or example: %q", got)
			}
		})
	}
}

func TestReportPreservesEarlierDecisionBytes(t *testing.T) {
	tempConfig(t)
	path := paths.MemorySub("reports", now().Format(dayLayout)+"-decisions.md")
	before := "# Decisions\n\n- prior   " + strings.Repeat("legacy  text ", 20) + "\n"
	write(t, path, before)
	files, err := appendReportLocked("review", ReportInput{Report: "# Report\nSummary.\n- item\n", Decisions: []string{strings.Repeat("new words ", 20)}})
	if err != nil {
		t.Fatal(err)
	}
	got := mustRead(t, path)
	if !strings.HasPrefix(got, before) {
		t.Errorf("earlier decision bytes changed: %q", got)
	}
	assertWrapped(t, got[len(before):])
	if got := mustRead(t, files[0]); got != "# Report\n\nSummary.\n\n- item\n" {
		t.Errorf("new report has unclean block boundaries: %q", got)
	}
}

func TestReportDecisionAppendRefusesFence(t *testing.T) {
	tempConfig(t)
	path := paths.MemorySub("reports", now().Format(dayLayout)+"-decisions.md")
	before := "# Decisions\n\n```\n"
	write(t, path, before)
	if _, err := appendReportLocked("review", ReportInput{Report: "# Report\n\nSummary.\n", Decisions: []string{"new"}}); err == nil {
		t.Fatal("decision hidden inside an unclosed fence")
	}
	if got := mustRead(t, path); got != before {
		t.Errorf("refused append changed bytes: %q", got)
	}
}
