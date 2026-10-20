package memory

import (
	"encoding/json"
	"errors"
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
	files, _ := filepath.Glob(filepath.Join(ReceiptsDir(), "*.json"))
	return files
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
	err := WithWritableBankLock(BankLockWait, func() error {
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
	err = WithWritableBankLock(BankLockWait, func() error {
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
	if err := WithWritableBankLock(BankLockWait, func() error {
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
	err := WithWritableBankLock(BankLockWait, recoverJournals)
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
	err = WithWritableBankLock(BankLockWait, recoverJournals)
	if err == nil || !strings.Contains(err.Error(), "does not match its file name") {
		t.Fatalf("forged id accepted: %v", err)
	}
	if _, err := os.Stat(forged); err != nil {
		t.Fatal("forged journal was moved or removed instead of refused")
	}
	assertDecoyUntouched(t, decoy)
}
