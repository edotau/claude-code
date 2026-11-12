package memory

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/textutil"
)

// captureStderr redirects os.Stderr to a file until the test ends; the returned func reads what was written so far.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = original; _ = f.Close() })
	return func() string { return mustRead(t, f.Name()) }
}

func readStore(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(StoreDir(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func journals(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(JournalDir(), "*.json"))
	return files
}

// commitLocked runs one batch under the writable bank lock, the shape every caller uses.
func commitLocked(t *testing.T, subject string, changes []Change) (Receipt, error) {
	t.Helper()
	var rec Receipt
	err := WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked(subject, changes)
		return err
	})
	return rec, err
}

// assertNoopBatch: a second identical batch changes no bytes, so there is no commit.
func assertNoopBatch(t *testing.T, first Receipt) {
	t.Helper()
	rec2, err := commitLocked(t, "import", []Change{{Rel: "repo/MEMORY.md", Data: []byte("- a\n")}})
	if err != nil || rec2.Commit != "" || len(rec2.Skipped) != 1 {
		t.Fatalf("no-op batch = %+v, %v", rec2, err)
	}
	if head, _ := StoreRepo().Head(); head != first.Commit {
		t.Fatalf("empty commit created: %s != %s", head, first.Commit)
	}
}

func TestCommitChangesLockedFreshImportCommits(t *testing.T) {
	store, decoy := storeFixture(t)
	rec, err := commitLocked(t, "import", []Change{
		{Rel: "repo/MEMORY.md", Data: []byte("- a\n")},
		{Rel: "repo/activeContext.md", Data: []byte("# Active\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Applied) != 2 || rec.Commit == "" || len(rec.Divergent)+len(rec.Skipped) != 0 {
		t.Fatalf("receipt = %+v", rec)
	}
	if !strings.Contains(strings.Join(rec.Preexisting, ","), ".gitignore") {
		t.Fatalf("seeded .gitignore must be swept as preexisting: %+v", rec.Preexisting)
	}
	st, _ := StoreRepo().Status()
	if len(st) != 0 {
		t.Fatalf("store dirty after commit: %+v", st)
	}
	if got := journals(t); len(got) != 0 {
		t.Fatalf("journal left behind: %v", got)
	}
	if _, err := os.Stat(filepath.Join(store, ".git", "hooks", "pre-push")); err != nil {
		t.Fatal(err)
	}
	assertNoopBatch(t, rec)
	assertDecoyUntouched(t, decoy)
}

// Delete: absent is skipped, present is removed and committed.
func TestCommitChangesLockedDeleteRules(t *testing.T) {
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "seed", []Change{{Rel: "r/new.md", Data: []byte("new\n")}}); err != nil {
		t.Fatal(err)
	}
	rec, err := commitLocked(t, "delete", []Change{
		{Rel: "r/gone.md", Delete: true},
		{Rel: "r/new.md", Delete: true},
	})
	if err != nil || strings.Join(rec.Skipped, ",") != "r/gone.md" || strings.Join(rec.Applied, ",") != "r/new.md" {
		t.Fatalf("delete batch = %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(store, "r", "new.md")); !os.IsNotExist(err) {
		t.Fatal("r/new.md not removed")
	}
	if out, _ := StoreRepo().run("ls-files", "r/new.md"); out != "" {
		t.Fatal("deletion not committed")
	}
}

// Regression: validateRel rejects an escaping rel, and a symlinked ANCESTOR directory, not just a symlinked
// leaf file — both must be refused before a journal is ever written (a symlink two levels up could otherwise
// carry the write anywhere on disk).
func TestCommitChangesLockedRefusesEscapeAndSymlinkAncestorBeforeJournal(t *testing.T) {
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "x", []Change{{Rel: "../out.md", Data: []byte("a")}}); err == nil {
		t.Fatal("escaping rel accepted")
	}
	if len(journals(t)) != 0 {
		t.Fatal("rejected batch wrote a journal")
	}
	if StoreRepo().Exists() {
		t.Fatal("rejected batch initialized the repo")
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(store, "repo")); err != nil {
		t.Fatal(err)
	}
	if _, err := commitLocked(t, "escape", []Change{{Rel: "repo/sub/MEMORY.md", Data: []byte("x\n")}}); err == nil {
		t.Fatal("commit accepted a write through a symlinked ancestor directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub", "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatal("bytes escaped through the symlinked ancestor")
	}
	if len(journals(t)) != 0 {
		t.Fatal("refused batch wrote a journal")
	}
}

func TestCommitChangesLockedSkipsSymlinkPreexisting(t *testing.T) {
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "seed", []Change{{Rel: "r/a.md", Data: []byte("a\n")}}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("secret-ish"), 0o600)
	if err := os.Symlink(outside, filepath.Join(store, "r", "link.md")); err != nil {
		t.Fatal(err)
	}
	rec, err := commitLocked(t, "settle", []Change{{Rel: "r/a.md", Data: []byte("b\n")}})
	if err != nil || len(rec.Preexisting) != 0 {
		t.Fatalf("symlink swept: %+v, %v", rec, err)
	}
	if out, _ := StoreRepo().run("ls-files", "r/link.md"); out != "" {
		t.Fatal("symlink was committed")
	}
}

// batchLocked runs one batch under the bank lock like commitLocked, but without a *testing.T.
func batchLocked(subject string, changes []Change) (rec Receipt, err error) {
	err = WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked(subject, changes)
		return err
	})
	return rec, err
}

// assertJournalSurvives checks exactly one applied journal for r/x.md remains and its bytes are on disk.
func assertJournalSurvives(t *testing.T) {
	t.Helper()
	js := journals(t)
	if len(js) != 1 {
		t.Fatalf("journal must survive a commit failure: %v", js)
	}
	var j journal
	data, err := os.ReadFile(js[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	if j.State != "applied" || len(j.Changes) != 1 || string(j.Changes[0].Data) != "x\n" {
		t.Fatalf("journal = %+v", j)
	}
	if readStore(t, "r/x.md") != "x\n" {
		t.Fatal("bytes not applied before the failed commit")
	}
}

func TestCommitChangesLockedRecoversAfterCommitFailure(t *testing.T) {
	store, decoy := storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	// Hooks are disabled; a held ref lock makes git commit itself fail.
	refLock := filepath.Join(store, ".git", "refs", "heads", "main.lock")
	write(t, refLock, "")
	_, err := batchLocked("batch", []Change{{Rel: "r/x.md", Data: []byte("x\n")}})
	if err == nil || errors.Is(err, errDivergent) {
		t.Fatalf("commit must fail under the held ref lock: %v", err)
	}
	assertJournalSurvives(t)
	if err := os.Remove(refLock); err != nil {
		t.Fatal(err)
	}
	// The next batch recovers the old journal as its own commit.
	rec, err := batchLocked("next", []Change{{Rel: "r/y.md", Data: []byte("y\n")}})
	if err != nil {
		t.Fatal(err)
	}
	assertRecovered(t, rec)
	assertDecoyUntouched(t, decoy)
}

// assertRecovered checks the old journal is retired, both batches are committed, and the store is clean.
func assertRecovered(t *testing.T, rec Receipt) {
	t.Helper()
	if len(journals(t)) != 0 {
		t.Fatal("journal not retired after recovery")
	}
	log, err := StoreRepo().run("log", "--format=%s")
	if err != nil || strings.TrimSpace(log) != "next\nbatch" {
		t.Fatalf("log = %q, %v", log, err)
	}
	if len(rec.Preexisting) != 0 {
		t.Fatalf("recovered batch's files leaked into the next batch as preexisting: %v", rec.Preexisting)
	}
	st, err := StoreRepo().Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 0 {
		t.Fatalf("dirty after recovery: %+v", st)
	}
}

// Regression: apply re-reads the live bytes right before mutating, so a native edit landing between plan
// (or capture) and the write is reported divergent, never overwritten; a late preimage match was never captured.
func TestApplyRechecksLiveBytesBeforeWrite(t *testing.T) {
	store, _ := storeFixture(t)
	if err := WithBankLock(BankLockWait, func() error {
		_, err := commitChangesLocked("seed", []Change{{Rel: "r/a.md", Data: []byte("v1\n")}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c := journalChange{Rel: "r/a.md", HasPreimage: true, PreimageSHA256: textutil.SHA256Hex([]byte("v1\n")), Data: []byte("v2\n")}
	repo := StoreRepo()
	live := filepath.Join(store, "r", "a.md")
	write(t, live, "edited after plan\n")
	assertApply(t, c, repo, outApply, "", "r/a.md", "planned apply over edited bytes")
	if readStore(t, "r/a.md") != "edited after plan\n" {
		t.Fatal("cached apply decision overwrote a native edit")
	}
	write(t, live, "v1\n")
	assertApply(t, c, repo, outSkip, "", "r/a.md", "late preimage match must diverge (never captured)")
	if readStore(t, "r/a.md") != "v1\n" {
		t.Fatal("uncaptured bytes were overwritten")
	}
	assertApply(t, c, repo, outApply, "r/a.md", "", "matching preimage")
	if readStore(t, "r/a.md") != "v2\n" {
		t.Fatal("matching preimage not applied")
	}
}

// assertApply applies c with the planned outcome into a fresh receipt and checks its Applied and Divergent rels.
func assertApply(t *testing.T, c journalChange, repo Repo, planned outcome, wantApplied, wantDivergent, label string) {
	t.Helper()
	var rec Receipt
	err := c.apply(repo, planned, &rec)
	if err != nil || strings.Join(rec.Applied, ",") != wantApplied || strings.Join(rec.Divergent, ",") != wantDivergent {
		t.Fatalf("%s: %+v, %v", label, rec, err)
	}
}

// recoverJournals finishes every unfinished journal and returns the scan error and per-journal failures joined.
func recoverJournals() error {
	skipped, err := recoverEach()
	return errors.Join(err, skipped)
}

// Regression: replayed journals are validated before anything is read or written.
func TestRecoverJournalsRefusesEscapingPaths(t *testing.T) {
	store, decoy := storeFixture(t)
	outside := filepath.Join(filepath.Dir(store), "escape.md")
	bad := journal{ID: "20000101-000000.000-00000000", Subject: "replay", State: "pending",
		Changes: []journalChange{{Rel: "../escape.md", Data: []byte("x\n")}}}
	if err := bad.save(); err != nil {
		t.Fatal(err)
	}
	err := WithBankLock(BankLockWait, recoverJournals)
	if err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("escaping replay accepted: %v", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("bytes escaped the store")
	}
	if StoreRepo().Exists() {
		t.Fatal("refused replay initialized the repo")
	}
	os.Remove(bad.path())
	assertForgedJournalRefused(t)
	assertDecoyUntouched(t, decoy)
}

func assertForgedJournalRefused(t *testing.T) {
	t.Helper()
	forged := filepath.Join(JournalDir(), "20000101-000000.000-11111111.json")
	if err := os.MkdirAll(JournalDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, forged, `{"id":"../../forged","subject":"replay","state":"pending","changes":[]}`)
	err := WithBankLock(BankLockWait, recoverJournals)
	if err == nil || !strings.Contains(err.Error(), "does not match its file name") {
		t.Fatalf("forged id accepted: %v", err)
	}
	if _, err := os.Stat(forged); err != nil {
		t.Fatal("forged journal was moved or removed instead of refused")
	}
}

// writeJournal seeds a pending one-file journal (extra fields merged in) under the state journal dir.
func writeJournal(t *testing.T, id string, extra map[string]any) {
	t.Helper()
	doc := map[string]any{
		"id": id, "subject": "seeded", "state": "pending",
		"changes": []map[string]any{{"rel": "r/seeded.md", "delete": false, "hasPreimage": false, "data": []byte("seeded\n")}},
	}
	maps.Copy(doc, extra)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(JournalDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(JournalDir(), id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nextBatch(t *testing.T) error {
	t.Helper()
	_, err := commitLocked(t, "next", []Change{{Rel: "r/next.md", Data: []byte("n\n")}})
	return err
}

// A schema-2 (newer) journal is never replayed: its target stays untouched and the file stays in place.
func TestRecoverSkipsNewerSchemaJournal(t *testing.T) {
	store, decoy := storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	id := "20260101-000000.000-aaaa"
	writeJournal(t, id, map[string]any{"schema": 2})

	_ = nextBatch(t) // reporting the newer journal as an error is allowed; replaying it is not
	if _, err := os.Stat(filepath.Join(store, "r", "seeded.md")); !os.IsNotExist(err) {
		t.Fatalf("newer journal was replayed (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(JournalDir(), id+".json")); err != nil {
		t.Fatalf("newer journal not left in place: %v", err)
	}
	assertDecoyUntouched(t, decoy)
}

// A v0 journal (no schema field) still recovers on the next batch.
func TestRecoverReplaysV0Journal(t *testing.T) {
	storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, "20260101-000000.000-bbbb", nil)

	if err := nextBatch(t); err != nil {
		t.Fatal(err)
	}
	if got := readStore(t, "r/seeded.md"); got != "seeded\n" {
		t.Fatalf("v0 journal not recovered: %q", got)
	}
	if len(journals(t)) != 0 {
		t.Fatalf("journal not retired: %v", journals(t))
	}
}

// Regression: a journal that can never finish must not wedge later batches or the journals behind it.
func TestCommitChangesLockedNotWedgedByPermanentlyFailingJournal(t *testing.T) {
	storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	stuck := "20260101-000000.000-aaaa"
	writeJournal(t, stuck, map[string]any{"schema": 2})
	writeJournal(t, "20260101-000001.000-bbbb", nil)

	if err := nextBatch(t); err != nil {
		t.Fatalf("stuck journal wedged the next batch: %v", err)
	}
	if got := readStore(t, "r/next.md"); got != "n\n" {
		t.Fatalf("new batch not committed: %q", got)
	}
	if got := readStore(t, "r/seeded.md"); got != "seeded\n" {
		t.Fatalf("journal behind the stuck one not recovered: %q", got)
	}
	if _, err := os.Stat(filepath.Join(JournalDir(), stuck+".json")); err != nil {
		t.Fatalf("stuck journal should stay on disk: %v", err)
	}
}

// A skipped older journal that replays after a newer commit to the same file diverges instead of clobbering.
func TestSkippedJournalReplayDivergesOnSameFile(t *testing.T) {
	storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := commitLocked(t, "newer", []Change{{Rel: "r/seeded.md", Data: []byte("newer\n")}}); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, "20260101-000000.000-aaaa", map[string]any{"changes": []map[string]any{{
		"rel": "r/seeded.md", "hasPreimage": true, "preimageSha256": textutil.SHA256Hex([]byte("old\n")), "data": []byte("stale\n"),
	}}})

	_, err := commitLocked(t, "after", []Change{{Rel: "r/other.md", Data: []byte("o\n")}})
	if err != nil {
		t.Fatalf("diverging journal must not wedge: %v", err)
	}
	if got := readStore(t, "r/seeded.md"); got != "newer\n" {
		t.Fatalf("stale journal clobbered the newer commit: %q", got)
	}
}

// TestCommitChangesLockedSkipsAnotherReposCredentialFile: Given a credential-shaped file outside the batch's dirs,
// When the batch commits, Then it succeeds, names the path once on stderr and never sweeps the file; inside, it refuses.
func TestCommitChangesLockedSkipsAnotherReposCredentialFile(t *testing.T) {
	storeFixture(t)
	write(t, filepath.Join(StoreDir(), "other", "note.md"), "key: "+fakeCredential()+"\n")
	readStderr := captureStderr(t)
	rec, err := commitLocked(t, "import", []Change{{Rel: "r/a.md", Data: []byte("a\n")}})
	stderr := readStderr()
	if err != nil {
		t.Fatalf("an unrelated credential-shaped file failed the batch: %v", err)
	}
	if slices.Contains(rec.Preexisting, "other/note.md") {
		t.Fatalf("credential-shaped file swept into the batch: %+v", rec.Preexisting)
	}
	if n := strings.Count(stderr, "other/note.md"); n != 1 || strings.Contains(stderr, fakeCredential()) {
		t.Fatalf("stderr must name the path once and no secret: %q", stderr)
	}
	if st, _ := StoreRepo().Status("other"); len(st) != 1 {
		t.Fatalf("the skipped file must stay uncommitted: %+v", st)
	}

	write(t, filepath.Join(StoreDir(), "r", "leak.md"), "key: "+fakeCredential()+"\n")
	if _, err := commitLocked(t, "import", []Change{{Rel: "r/b.md", Data: []byte("b\n")}}); err == nil ||
		!strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the batch's dir must be refused, got %v", err)
	}
}

// storeFixture isolates the config dir and git's global config, and sets a HOSTILE inherited git env pointing at a
// decoy repo with staged work; every test asserts the decoy is untouched.
func storeFixture(t *testing.T) (store string, decoy Repo) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	decoy = Repo{Dir: filepath.Join(t.TempDir(), "decoy")}
	if err := decoy.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(decoy.Dir, "staged.md"), "staged, never committed\n")
	if _, err := decoy.run("add", "-A", "--", "staged.md", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy.Dir, ".git", "index"))
	return StoreDir(), decoy
}

// assertDecoyUntouched: the hostile GIT_* env must never retarget a store operation at the decoy.
func assertDecoyUntouched(t *testing.T, decoy Repo) {
	t.Helper()
	if head, _ := decoy.Head(); head != "" {
		t.Fatalf("decoy gained a commit %s", head)
	}
	st, err := decoy.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0] != (statusEntry{"A ", ".gitignore"}) || st[1] != (statusEntry{"A ", "staged.md"}) {
		t.Fatalf("decoy index changed: %+v", st)
	}
}

func TestEnsureInitsNestedRepoWithGuards(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if r.Exists() {
		t.Fatal("store repo must not exist before Ensure")
	}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if !r.Exists() {
		t.Fatal("Ensure did not create <store>/.git")
	}
	hookPath := filepath.Join(store, ".git", "hooks", "pre-push")
	hook, err := os.Stat(hookPath)
	if err != nil || runtime.GOOS != "windows" && hook.Mode()&0o100 == 0 { // Windows has no exec bit
		t.Fatalf("pre-push hook missing or not executable: %v", err)
	}
	assertGitignoreSeeded(t, store)
	// Idempotent: a user's own .gitignore edit survives a second Ensure.
	write(t, filepath.Join(store, ".gitignore"), "custom\n")
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(store, ".gitignore")); string(data) != "custom\n" {
		t.Fatalf(".gitignore overwritten: %q", data)
	}
	assertFreshRepoEmpty(t, r)
	assertPrePushRefuses(t, hookPath)
	assertDecoyUntouched(t, decoy)
}

// assertGitignoreSeeded checks Ensure wrote the store's .gitignore with access.log and without *.lock.
func assertGitignoreSeeded(t *testing.T, store string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store, ".gitignore"))
	if err != nil || !strings.Contains(string(data), "access.log") {
		t.Fatalf(".gitignore not seeded: %v", err)
	}
	if strings.Contains(string(data), "*.lock") {
		t.Error(".gitignore still lists *.lock; locks moved to state/, never tracked in the store")
	}
}

// assertFreshRepoEmpty checks a just-ensured repo has no HEAD, no log and no remotes.
func assertFreshRepoEmpty(t *testing.T, r Repo) {
	t.Helper()
	if head, _ := r.Head(); head != "" {
		t.Fatalf("fresh repo has HEAD %q", head)
	}
	if remotes, err := r.remotes(); err != nil || len(remotes) != 0 {
		t.Fatalf("fresh repo remotes() = %v, %v; want none", remotes, err)
	}
}

// assertPrePushRefuses runs the hook and requires exit status 1.
func assertPrePushRefuses(t *testing.T, hookPath string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("running the pre-push hook needs sh on PATH")
	}
	cmd := exec.Command("sh", hookPath)
	if err := cmd.Run(); err == nil {
		t.Fatal("pre-push hook must exit nonzero (refuses every push)")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("pre-push hook exit = %v, want exit 1", err)
	}
}

func TestEnsureRefusesPointerFileAndRemotes(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	r := Repo{Dir: store}
	write(t, filepath.Join(store, ".git"), "gitdir: /elsewhere\n")
	if err := r.Ensure(); err == nil {
		t.Fatal("Ensure accepted a gitdir pointer file")
	}
	os.Remove(filepath.Join(store, ".git"))
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run("remote", "add", "origin", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	err := r.Ensure()
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("Ensure with a remote = %v, want error naming origin", err)
	}
}

func TestCommitStagesOnlyNamedPathsAndSkipsEmpty(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "repo", "MEMORY.md"), "- fact\n")
	write(t, filepath.Join(store, "repo", "other.md"), "unrelated\n")
	sha, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "settle", "")
	if err != nil || !committed || sha == "" {
		t.Fatalf("Commit = %q, %v, %v", sha, committed, err)
	}
	st, _ := r.Status()
	if len(st) != 2 || st[0].Path != ".gitignore" || st[1].Path != "repo/other.md" {
		t.Fatalf("unnamed paths must stay dirty: %+v", st)
	}
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "again", ""); err != nil || committed {
		t.Fatalf("unchanged commit = %v, %v; want no empty commit", committed, err)
	}
	assertDecoyUntouched(t, decoy)
}

// Absent + untracked is dropped; absent + tracked stages a deletion.
func TestCommitDropsUntrackedAndStagesDeletion(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "repo", "MEMORY.md"), "- fact\n")
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md"}, "settle", ""); err != nil || !committed {
		t.Fatalf("seed commit = %v, %v", committed, err)
	}
	os.Remove(filepath.Join(store, "repo", "MEMORY.md"))
	if _, committed, err := r.Commit([]string{"repo/MEMORY.md", "never/existed.md"}, "delete", "body"); err != nil || !committed {
		t.Fatalf("deletion commit = %v, %v", committed, err)
	}
	log, err := r.run("log", "--format=%s")
	if err != nil || strings.Count(log, "\n") != 1 || !strings.HasPrefix(log, "delete\n") {
		t.Fatalf("log = %q, %v", log, err)
	}
	if head, _ := r.Head(); head == "" {
		t.Fatal("Head empty after commits")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsBadPathsAndSymlinks(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"", ".", "..", "/abs/x.md", "a/../b.md", ".git/config", "../escape.md", "a//b.md"} {
		if _, _, err := r.Commit([]string{rel}, "s", ""); err == nil {
			t.Errorf("Commit accepted %q", rel)
		}
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(store, "link.md")); err != nil {
		assertDecoyUntouched(t, decoy)
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, _, err := r.Commit([]string{"link.md"}, "s", ""); err == nil {
		t.Error("Commit accepted a symlink")
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitRejectsSecretsByNameOnly(t *testing.T) {
	store, decoy := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	const token = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef"
	write(t, filepath.Join(store, "leak.md"), "token "+token+"\n")
	_, _, err := r.Commit([]string{"leak.md"}, "s", "")
	if err == nil || !strings.Contains(err.Error(), "leak.md") || strings.Contains(err.Error(), token) {
		t.Fatalf("secret rejection = %v; want path named, bytes absent", err)
	}
	write(t, filepath.Join(store, "prose.md"), "ask-before-changing-appearance-of-anything-here\n")
	if _, committed, err := r.Commit([]string{"prose.md"}, "s", ""); err != nil || !committed {
		t.Fatalf("kebab prose rejected: %v", err)
	}
	if head, _ := r.Head(); head == "" {
		t.Fatal("no commit after prose")
	}
	assertDecoyUntouched(t, decoy)
}

// Regression: checkRoot's commondir guard — a .git directory (not a pointer file, so Ensure's own
// foreign-worktree check never fires) whose "commondir" file redirects refs/objects to another repo
// must still be rejected before the store receives a byte.
func TestCheckRootRejectsSharedCommondir(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	other := filepath.Join(t.TempDir(), "other")
	if err := (Repo{Dir: other}).Ensure(); err != nil {
		t.Fatal(err)
	}
	r := Repo{Dir: store}
	gitDir := filepath.Join(store, ".git")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run("init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	// Redirect this .git's commondir to the other repo's .git — refs/objects would be shared.
	rel, err := filepath.Rel(gitDir, filepath.Join(other, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Join(other, ".git"), rel} {
		write(t, filepath.Join(gitDir, "commondir"), target+"\n")
		err = r.checkRoot(gitDir)
		if err == nil || !strings.Contains(err.Error(), "commondir") {
			t.Fatalf("checkRoot with commondir %q = %v, want a commondir-sharing error", target, err)
		}
	}
	os.Remove(filepath.Join(gitDir, "commondir"))
	if err := r.checkRoot(gitDir); err != nil {
		t.Fatalf("checkRoot without commondir = %v", err)
	}
}

func TestStatusParsesRenamesAndUntrackedInDirs(t *testing.T) {
	t.Parallel()
	store := filepath.Join(t.TempDir(), "memory")
	r := Repo{Dir: store}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "a.md"), "same body for rename detection, long enough to match\n")
	if _, _, err := r.Commit([]string{"a.md", ".gitignore"}, "base", ""); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(store, "a.md"), filepath.Join(store, "b.md"))
	write(t, filepath.Join(store, "deep", "dir", "c.md"), "c\n")
	if _, err := r.run("add", "-A", "--", "a.md", "b.md"); err != nil {
		t.Fatal(err)
	}
	st, err := r.Status()
	if err != nil {
		t.Fatal(err)
	}
	want := []statusEntry{{"R ", "b.md"}, {"D ", "a.md"}, {"??", "deep/dir/c.md"}}
	if !slices.Equal(st, want) {
		t.Fatalf("Status = %+v, want %+v", st, want)
	}
	if st, _ = r.Status("deep"); len(st) != 1 || st[0].Path != "deep/dir/c.md" {
		t.Fatalf("Status(pathspec) = %+v", st)
	}
}

func TestGitOutSanitizesEnv(t *testing.T) {
	_, decoy := storeFixture(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := (Repo{Dir: other}).Ensure(); err != nil {
		t.Fatal(err)
	}
	top, err := gitOut(other, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(top); got != mustEval(t, other) {
		t.Fatalf("gitOut followed the inherited GIT_DIR: %q", top)
	}
	if _, err := gitOut(t.TempDir(), "rev-parse", "--show-toplevel"); err == nil {
		t.Fatal("gitOut outside a repo must fail, not fall through to GIT_DIR")
	}
	assertDecoyUntouched(t, decoy)
}

func TestExitCodeDistinguishesDiff(t *testing.T) {
	t.Parallel()
	if exitCode(errors.New("plain")) != -1 {
		t.Fatal("non-exit error must be -1")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Store git calls run with -C: from a cwd inside the store, pathspecs and output stay relative to the store root.
func TestRepoRunIgnoresProcessCwd(t *testing.T) {
	store, _ := storeFixture(t)
	r := Repo{Dir: store}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "proj", "activeContext.md"), "x\n")
	t.Chdir(filepath.Join(store, "proj"))
	st, err := r.Status("proj/activeContext.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Path != "proj/activeContext.md" {
		t.Fatalf("Status from a store subdir = %+v, want proj/activeContext.md", st)
	}
}

func TestStoreGitProbesBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec \""+sleep+"\" 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	oldTimeout := gitTimeout
	gitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitTimeout = oldTimeout })

	t.Run("git toplevel", func(t *testing.T) {
		started := time.Now()
		if got := GitToplevel(t.TempDir()); got != "" {
			t.Errorf("timed-out toplevel probe = %q, want empty", got)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Errorf("toplevel probe took %v with timeout %v", elapsed, gitTimeout)
		}
	})
}

// TestHeadDistinguishesUnbornFromFailure: an unborn HEAD is ("", nil); any other git failure is returned.
func TestHeadDistinguishesUnbornFromFailure(t *testing.T) {
	storeFixture(t)
	r := Repo{Dir: filepath.Join(t.TempDir(), "repo")}
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	if head, err := r.Head(); err != nil || head != "" {
		t.Fatalf("unborn Head = %q, %v, want \"\", nil", head, err)
	}
	if _, err := (Repo{Dir: filepath.Join(t.TempDir(), "missing")}).Head(); err == nil {
		t.Fatal("Head on a non-repo = nil error, want the git failure")
	}
}

// credentialPositives are real secret shapes, assembled from parts so no secret-shaped literal sits in the source.
func credentialPositives() map[string]string {
	return map[string]string{
		"sk-proj":      "sk-" + "proj-" + strings.Repeat("a1B2", 10),
		"sk-ant":       "sk-" + "ant-api03-" + strings.Repeat("x9Y_", 6),
		"sk classic":   "sk-" + strings.Repeat("a1B2c", 4),
		"dapi":         "dapi" + strings.Repeat("0f", 16),
		"jwt":          "ey" + "J" + strings.Repeat("hbGciOi", 3) + "." + strings.Repeat("eyJzdWIi", 3),
		"nvapi":        "nvapi-" + strings.Repeat("Ab1-", 7),
		"ghp":          "gh" + "p_" + strings.Repeat("A1b2", 9),
		"github_pat":   "github" + "_pat_" + strings.Repeat("11AB", 6) + "_" + strings.Repeat("cd34", 15),
		"akia":         "AK" + "IA" + strings.Repeat("Q7", 8),
		"rsa private":  "-----BEGIN " + "RSA PRIVATE KEY-----",
		"bare private": "-----BEGIN " + "PRIVATE KEY-----",
	}
}

// credentialNegatives were the loose pattern's false positives: kebab prose, public material, too-short fragments.
func credentialNegatives() map[string]string {
	return map[string]string{
		"kebab task":  "task-abcdefghij12",
		"kebab mask":  "mask-0123456789ab",
		"certificate": "-----BEGIN " + "CERTIFICATE-----",
		"pgp public":  "-----BEGIN " + "PGP PUBLIC KEY BLOCK-----",
		"short dapi":  "dapi" + "0123456789ab",
		"short sk":    "sk-" + "short12345",
		"akia inside": "SLOV" + "AKIA" + strings.Repeat("BC", 8),
	}
}

func TestCredentialPatternMatchesSecretShapes(t *testing.T) {
	for name, v := range credentialPositives() {
		if !textutil.CredentialRE.MatchString("note: " + v + " here") {
			t.Errorf("%s: secret shape not matched", name)
		}
	}
}

func TestCredentialPatternSkipsFalsePositives(t *testing.T) {
	for name, v := range credentialNegatives() {
		if textutil.CredentialRE.MatchString("note: " + v + " here") {
			t.Errorf("%s: %q flagged as a credential", name, v)
		}
	}
}

// TestDoctorAndCommitGateAgree drives doctor's statBank and the commit gate's checkStageContent on one file each.
func TestDoctorAndCommitGateAgree(t *testing.T) {
	cases := map[string]bool{}
	for _, v := range credentialPositives() {
		cases[v] = true
	}
	for _, v := range credentialNegatives() {
		cases[v] = false
	}
	for v, want := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "progress.md")
		write(t, path, "- note "+v+"\n")
		doctorFlags := len(statBank(dir).Credentials) > 0
		gateRefuses := checkStageContent(path, "progress.md") != nil
		if doctorFlags != want || gateRefuses != want {
			t.Errorf("%q: doctor flags %v, gate refuses %v, want both %v", v, doctorFlags, gateRefuses, want)
		}
	}
}

// Regression: the commit gate missed shapes the config-history scanner holds back (Tavily, Google, Groq, AWS STS, Slack).
func TestCommitGateRefusesHistoryScannerShapes(t *testing.T) {
	cases := map[string]string{
		"tavily":  "tvly-" + strings.Repeat("Fk", 12),
		"google":  "AI" + "za" + strings.Repeat("Fk1", 11) + "Fk",
		"groq":    "gsk_" + strings.Repeat("Fk", 15),
		"aws sts": "AS" + "IA" + strings.Repeat("FK", 8),
		"slack":   "xoxb-" + "1234567890-FAKEFAKEFAKE",
	}
	for name, v := range cases {
		path := filepath.Join(t.TempDir(), "progress.md")
		write(t, path, "- fact "+v+"\n")
		if checkStageContent(path, "progress.md") == nil {
			t.Errorf("%s: commit gate accepted a credential-shaped fact", name)
		}
	}
}

func TestCommitStagesGlobNamedFilesLiterally(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "why?.md"), "glob-named\n")
	write(t, filepath.Join(store, "whyx.md"), "sibling the glob would match\n")
	if _, committed, err := r.Commit([]string{"why?.md"}, "literal", ""); err != nil || !committed {
		t.Fatalf("Commit(why?.md) = %v, %v; a glob pathspec staged the sibling", committed, err)
	}
	if got := hosttest.In(t, store, "ls-tree", "--name-only", "HEAD"); got != "why?.md\n" {
		t.Fatalf("HEAD tree = %q, want only why?.md", got)
	}
}

func TestStoreGitIgnoresTheUsersGlobalExcludes(t *testing.T) {
	store, _ := storeFixture(t)
	r := StoreRepo()
	if err := r.Ensure(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "ignore"), "*.json\nreports/\n")
	write(t, filepath.Join(store, "receipts", "x.json"), "{}\n")
	write(t, filepath.Join(store, "reports", "a.md"), "report\n")
	st, err := r.Status()
	if err != nil || !slices.Contains(st, statusEntry{"??", "receipts/x.json"}) || !slices.Contains(st, statusEntry{"??", "reports/a.md"}) {
		t.Fatalf("a global ignore hid store files from Status: %+v, %v", st, err)
	}
	if _, committed, err := r.Commit([]string{"receipts/x.json", "reports/a.md"}, "receipts", ""); err != nil || !committed {
		t.Fatalf("a global ignore broke Commit: %v, %v", committed, err)
	}
}
