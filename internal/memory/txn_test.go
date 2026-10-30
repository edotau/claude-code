package memory

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func receipts(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(StoreDir(), "receipts", "*.json"))
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

func TestCommitChangesLockedFreshImportCommitsWithReceipt(t *testing.T) {
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
	if got := receipts(t); len(got) != 1 {
		t.Fatalf("receipts = %v", got)
	}
	var onDisk Receipt
	if err := json.Unmarshal([]byte(readStore(t, "receipts/"+rec.ID+".json")), &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.ID != rec.ID || onDisk.Commit != "" || len(onDisk.Applied) != 2 {
		t.Fatalf("tracked receipt = %+v", onDisk)
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
	// A second identical batch changes no bytes: no receipt, no commit.
	rec2, err := commitLocked(t, "import", []Change{{Rel: "repo/MEMORY.md", Data: []byte("- a\n"), Checked: true}})
	if err != nil || rec2.Commit != "" || len(rec2.Skipped) != 1 {
		t.Fatalf("no-op batch = %+v, %v", rec2, err)
	}
	if head, _ := StoreRepo().Head(); head != rec.Commit {
		t.Fatalf("empty commit created: %s != %s", head, rec.Commit)
	}
	if got := receipts(t); len(got) != 1 {
		t.Fatalf("no-op batch wrote a receipt: %v", got)
	}
	assertDecoyUntouched(t, decoy)
}

func TestCommitChangesLockedPreimageRules(t *testing.T) {
	store, _ := storeFixture(t)
	if _, err := commitLocked(t, "seed", []Change{{Rel: "r/a.md", Data: []byte("v1\n")}, {Rel: "r/b.md", Data: []byte("v1\n")}}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, "r", "b.md"), "edited by native\n")
	write(t, filepath.Join(store, "r", "native.md"), "native topic\n")
	rec, err := commitLocked(t, "settle", []Change{
		{Rel: "r/a.md", Data: []byte("v2\n"), Preimage: []byte("v1\n"), Checked: true},
		{Rel: "r/b.md", Data: []byte("v2\n"), Preimage: []byte("v1\n"), Checked: true},
		{Rel: "r/new.md", Data: []byte("new\n"), Checked: true},
		{Rel: "r/also-new.md", Data: []byte("x\n"), Preimage: nil, Checked: true},
	})
	if !errors.Is(err, ErrDivergent) {
		t.Fatalf("err = %v, want ErrDivergent", err)
	}
	if strings.Join(rec.Applied, ",") != "r/a.md,r/new.md,r/also-new.md" || strings.Join(rec.Divergent, ",") != "r/b.md" {
		t.Fatalf("receipt = %+v", rec)
	}
	if strings.Join(rec.Preexisting, ",") != "r/native.md" {
		t.Fatalf("preexisting = %v", rec.Preexisting)
	}
	if readStore(t, "r/b.md") != "edited by native\n" {
		t.Fatal("divergent bytes were overwritten")
	}
	if readStore(t, "r/a.md") != "v2\n" {
		t.Fatal("matching preimage not applied")
	}
	st, _ := StoreRepo().Status()
	if len(st) != 1 || st[0].Path != "r/b.md" {
		t.Fatalf("only the divergent edit may stay dirty: %+v", st)
	}
	// Delete: absent is skipped, present with matching preimage is removed and committed.
	rec, err = commitLocked(t, "delete", []Change{
		{Rel: "r/gone.md", Delete: true, Checked: true},
		{Rel: "r/new.md", Delete: true, Preimage: []byte("new\n"), Checked: true},
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

func TestCommitChangesLockedRecoversAfterCommitFailure(t *testing.T) {
	store, decoy := storeFixture(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	// Hooks are disabled; a held ref lock makes git commit itself fail.
	refLock := filepath.Join(store, ".git", "refs", "heads", "main.lock")
	write(t, refLock, "")
	var rec Receipt
	err := WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked("batch", []Change{{Rel: "r/x.md", Data: []byte("x\n"), Checked: true}})
		return err
	})
	if err == nil || errors.Is(err, ErrDivergent) {
		t.Fatalf("commit must fail under the held ref lock: %v", err)
	}
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
	if err := os.Remove(refLock); err != nil {
		t.Fatal(err)
	}
	// The next batch recovers the old journal as its own commit, without a duplicate receipt.
	err = WithBankLock(BankLockWait, func() error {
		var err error
		rec, err = commitChangesLocked("next", []Change{{Rel: "r/y.md", Data: []byte("y\n")}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(journals(t)) != 0 {
		t.Fatal("journal not retired after recovery")
	}
	log, err := StoreRepo().Log(10)
	if err != nil || len(log) != 2 || !strings.HasSuffix(log[0], " next") || !strings.HasSuffix(log[1], " batch") {
		t.Fatalf("Log = %v, %v", log, err)
	}
	if got := receipts(t); len(got) != 2 {
		t.Fatalf("receipts = %v, want one per batch", got)
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
	assertDecoyUntouched(t, decoy)
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
	c := journalChange{Rel: "r/a.md", HasPreimage: true, PreimageSHA256: sha256Hex([]byte("v1\n")), Data: []byte("v2\n")}
	repo := StoreRepo()
	live := filepath.Join(store, "r", "a.md")
	write(t, live, "edited after plan\n")
	var rec Receipt
	if err := c.apply(repo, outApply, &rec); err != nil || strings.Join(rec.Divergent, ",") != "r/a.md" || len(rec.Applied) != 0 {
		t.Fatalf("planned apply over edited bytes: %+v, %v", rec, err)
	}
	if readStore(t, "r/a.md") != "edited after plan\n" {
		t.Fatal("cached apply decision overwrote a native edit")
	}
	write(t, live, "v1\n")
	rec = Receipt{}
	if err := c.apply(repo, outSkip, &rec); err != nil || strings.Join(rec.Divergent, ",") != "r/a.md" {
		t.Fatalf("late preimage match must diverge (never captured): %+v, %v", rec, err)
	}
	if readStore(t, "r/a.md") != "v1\n" {
		t.Fatal("uncaptured bytes were overwritten")
	}
	rec = Receipt{}
	if err := c.apply(repo, outApply, &rec); err != nil || strings.Join(rec.Applied, ",") != "r/a.md" {
		t.Fatalf("matching preimage: %+v, %v", rec, err)
	}
	if readStore(t, "r/a.md") != "v2\n" {
		t.Fatal("matching preimage not applied")
	}
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
	forged := filepath.Join(JournalDir(), "20000101-000000.000-11111111.json")
	if err := os.MkdirAll(JournalDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, forged, `{"id":"../../forged","subject":"replay","state":"pending","changes":[]}`)
	err = WithBankLock(BankLockWait, recoverJournals)
	if err == nil || !strings.Contains(err.Error(), "does not match its file name") {
		t.Fatalf("forged id accepted: %v", err)
	}
	if _, err := os.Stat(forged); err != nil {
		t.Fatal("forged journal was moved or removed instead of refused")
	}
	assertDecoyUntouched(t, decoy)
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
