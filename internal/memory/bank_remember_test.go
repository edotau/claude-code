package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// TestRememberUnknownTypeErrors: an unmapped --type is refused, naming the valid set.
func TestRememberUnknownTypeErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Remember(root, "bogus", "a fact", time.Now()); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("Remember with an unknown type = %v, want an error naming the valid types", err)
	}
}

// TestRememberEmptyFactErrors: whitespace-only input is refused before any write.
func TestRememberEmptyFactErrors(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Remember(root, "feedback", "   ", time.Now()); err == nil {
		t.Fatal("Remember with an empty fact = nil error, want one")
	}
}

func TestRememberFreshRepoKeepsItsStore(t *testing.T) {
	tempConfig(t)
	root := gitRepo(t, canonTemp(t, t.TempDir()), "fresh")
	dir, err := RepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := Remember(root, "feedback", "keep fresh repository facts discoverable", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := Dir(root); got != dir {
		t.Fatalf("Remember orphaned its store: directory changed from %s to %s", dir, got)
	}
	origin, err := readOrigin(dir)
	if err != nil || !origin.Accepts(root) {
		t.Fatalf("Remember did not record the repository origin: %+v, %v", origin, err)
	}
	hits, err := corpusBlocks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if blockID(hit.Block) == id {
			return
		}
	}
	t.Fatalf("remembered id %s is absent from the repository corpus", id)
}

// TestRememberProjectTypeWritesDatedDecisionLogEntryAtTop: a "project" fact lands as a dated,
// newest-first heading in decisionLog.md, and the returned id round-trips through recall.
func TestRememberProjectTypeWritesDatedDecisionLogEntryAtTop(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	id, err := Remember(root, "project", "adopt bbolt for the memory store", now)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned an empty id")
	}
	data, err := os.ReadFile(filepath.Join(Dir(root), "decisionLog.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "## 2026-10-23 — adopt bbolt for the memory store") {
		t.Errorf("decisionLog.md missing the dated heading: %q", body)
	}
	if !strings.Contains(body, "<!-- id:"+id+" -->") {
		t.Errorf("decisionLog.md missing the returned id %s: %q", id, body)
	}
	assertLaterProjectEntryFirst(t, root, id, now)
}

func assertLaterProjectEntryFirst(t *testing.T, root, id string, now time.Time) {
	t.Helper()
	// Round-trip: a second, later entry must still land ABOVE the first (newest-first).
	id2, err := Remember(root, "project", "a second, later decision", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(Dir(root), "decisionLog.md"))
	first := strings.Index(string(data), id2)
	second := strings.Index(string(data), id)
	if first == -1 || second == -1 || first > second {
		t.Errorf("newer entry (%s) did not land above the older one (%s): %q", id2, id, data)
	}
}

// TestRememberNonProjectTypesWriteConventionsBullet: user/feedback/reference all land as a labeled
// bullet in conventions.md, each carrying a persisted id.
func TestRememberNonProjectTypesWriteConventionsBullet(t *testing.T) {
	for _, kind := range []string{"user", "feedback", "reference"} {
		t.Run(kind, func(t *testing.T) {
			tempConfig(t)
			root := repoRoot(t, "proj")
			id, err := Remember(root, kind, "prefer early returns over deep nesting", time.Now())
			if err != nil {
				t.Fatalf("Remember(%s): %v", kind, err)
			}
			data, err := os.ReadFile(filepath.Join(Dir(root), "conventions.md"))
			if err != nil {
				t.Fatal(err)
			}
			label := strings.ToUpper(kind[:1]) + kind[1:]
			body := string(data)
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
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "decisionLog.md") // a directory: ReadFile fails with EISDIR
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRememberFact(dir, "decisionLog.md", "project", "x", time.Now()); err == nil {
		t.Fatal("writeRememberFact on an unreadable log = nil error, want the read error")
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("unreadable log was replaced: %v", err)
	}
}

// TestLastStampedIDExactMatchAndDirection: a longer older bullet sharing the prefix is skipped, a multi-line
// fact matches on its first line, and an appended file resolves to the last match.
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

// TestRememberReadsItsFileOnceBeforeSettle: the fact is stamped in memory and written once; only settle reads it again.
func TestRememberReadsItsFileOnceBeforeSettle(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	reads := countBankReads(t)
	id, err := Remember(root, "project", "read the log once", now)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(root), "decisionLog.md")
	if n := reads[path]; n != 2 {
		t.Errorf("Remember read decisionLog.md %d times, want 2 (its write, then settle)", n)
	}
	if body := mustRead(t, path); !strings.Contains(body, "## 2026-10-23 — read the log once <!-- id:"+id+" -->") {
		t.Errorf("decisionLog.md lacks the stamped fact with returned id %q: %q", id, body)
	}
}

// TestRememberSeedsMissingFileFromScaffold: a fact into an absent conventions.md lands under the scaffold header,
// not appendBlocks' "(archived)" one.
func TestRememberSeedsMissingFileFromScaffold(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	if _, err := Remember(root, "user", "likes tea", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := mustRead(t, filepath.Join(Dir(root), "conventions.md"))
	scaffold := strings.TrimRight(templates()["conventions.md"], "\n")
	if strings.Contains(body, "(archived)") || !strings.HasPrefix(body, scaffold) {
		t.Errorf("conventions.md = %q, want it to start with the scaffold %q", body, scaffold)
	}
}

// TestRememberRefusesFactItCannotStamp: a fact that would get no id of its own errors and leaves the file untouched.
func TestRememberRefusesFactItCannotStamp(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	path := filepath.Join(Dir(root), "conventions.md")
	for _, c := range []struct{ name, body, fact string }{
		{"unclosed fence above", "# Conventions\n\n- a rule\n\n```go\nx := 1\n", "fact after the fence"},
		{"id marker in the fact", "# Conventions\n\n- a rule\n", "quote <!-- id:deadbeef --> here"},
	} {
		write(t, path, c.body)
		if _, err := Remember(root, "feedback", c.fact, time.Now()); err == nil {
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
	root := repoRoot(t, "proj")
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
	_, err := Remember(root, "feedback", "settle in the same critical section", time.Now())
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
	lock := filepath.Join(StoreDir(), ".git", "index.lock")
	t.Cleanup(func() { _ = os.Remove(lock) })
	return func() {
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Error(err)
		}
	}
}

// TestRememberSettleFailureRollsBackTheWrite: a failed settle leaves the bank file as it was, so a retry is not a duplicate.
func TestRememberSettleFailureRollsBackTheWrite(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	seedConventionBlock(t, root)
	path := filepath.Join(Dir(root), "conventions.md")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := afterRememberWrite
	t.Cleanup(func() { afterRememberWrite = orig })
	afterRememberWrite = lockStoreIndex(t)
	const fact = "retry after a failed settle"
	if _, err := Remember(root, "feedback", fact, time.Now()); err == nil || !strings.Contains(err.Error(), "settle: ") {
		t.Fatalf("Remember with a failing settle = %v, want an error naming the settle", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("failed Remember left the bank changed:\nbefore %q\nafter  %q", before, after)
	}
	afterRememberWrite = orig
	_ = os.Remove(filepath.Join(StoreDir(), ".git", "index.lock"))
	if _, err := Remember(root, "feedback", fact, time.Now()); err != nil {
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
			dir := t.TempDir()
			report := filepath.Join(dir, "reports", "2026-11-08-review.md")
			decisions := filepath.Join(dir, "reports", "2026-11-08-decisions.md")
			if previous != "" {
				write(t, report, previous)
			}
			if err := os.MkdirAll(decisions, 0o700); err != nil {
				t.Fatal(err)
			}
			in := ReportInput{Report: "# Review\n\nUnique report body.", Decisions: []string{"Keep it."}}
			paths, err := appendReport(dir, "review", in)
			if err == nil || len(paths) != 0 {
				t.Fatalf("failed decisions write = %v, %v; want no paths and an error", paths, err)
			}
			assertReportRolledBack(t, report, previous)
			if err := os.Remove(decisions); err != nil {
				t.Fatal(err)
			}
			if _, err := appendReport(dir, "review", in); err != nil {
				t.Fatalf("retry report: %v", err)
			}
			assertReportTextOccursOnce(t, report, "Unique report body.")
			assertReportTextOccursOnce(t, decisions, "Keep it.")
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
			dir := t.TempDir()
			paths, err := appendReport(dir, label, ReportInput{
				Report:    "# Review\n\nReport body.\n",
				Decisions: []string{"Keep reports separate from decisions."},
			})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("appendReport(%q) = %v, %v; want reserved-label error", label, paths, err)
			}
			if len(paths) != 0 {
				t.Fatalf("rejected report returned paths: %v", paths)
			}
			if _, err := os.Stat(filepath.Join(dir, "reports")); !os.IsNotExist(err) {
				t.Fatalf("rejected label created reports directory: %v", err)
			}
		})
	}
}

// A failed store commit rolls back both appends, so a retry never duplicates the report or its decisions.

// not a repo: commitStore fails
