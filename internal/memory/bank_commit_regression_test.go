package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCommitChangesReturnsJournalScanErrors(t *testing.T) {
	isolatedConfig1(t)
	dir := JournalDir()
	write(t, filepath.Join(dir, "pending.json"), "{}\n")
	denyBankAccess(t, dir)
	_, err := batchLocked("must not bypass recovery", nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("transaction = %v, want journal scan permission error", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != dir {
		t.Fatalf("transaction bypassed journal listing and failed later: %v", err)
	}
	if StoreRepo().Exists() {
		t.Fatal("transaction initialized the store despite failed journal recovery")
	}
}

func seedRecoveryFact(t *testing.T) string {
	t.Helper()
	store, _ := storeFixture(t)
	if _, err := batchLocked("seed", []Change{{Rel: "r/fact.md", Data: []byte("preserve me\n")}}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDeleteTransactionRecoversFailedCommit(t *testing.T) {
	store := seedRecoveryFact(t)
	lock := filepath.Join(store, ".git", "refs", "heads", "main.lock")
	write(t, lock, "")
	_, err := batchLocked("delete", []Change{{Rel: "r/fact.md", Delete: true}})
	if err == nil || errors.Is(err, errDivergent) {
		t.Fatalf("held ref lock must fail the deletion commit: %v", err)
	}
	if len(journals(t)) != 1 {
		t.Fatal("failed deletion did not retain its journal")
	}
	if staged, err := StoreRepo().run("diff", "--cached", "--name-only"); err != nil || staged != "" {
		t.Fatalf("failed deletion left the index staged: %q, %v", staged, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := batchLocked("next", []Change{{Rel: "r/next.md", Data: []byte("next\n")}}); err != nil {
		t.Fatalf("next transaction could not recover deletion: %v", err)
	}
	assertDeletionRecovered(t)
}

func assertDeletionRecovered(t *testing.T) {
	t.Helper()
	if len(journals(t)) != 0 {
		t.Fatal("recovery left a journal behind")
	}
	if out, err := StoreRepo().run("ls-tree", "--name-only", "HEAD", "--", "r/fact.md"); err != nil || out != "" {
		t.Fatalf("recovered deletion still present at HEAD: %q, %v", out, err)
	}
	if status, err := StoreRepo().Status(); err != nil || len(status) != 0 {
		t.Fatalf("recovered store is dirty: %+v, %v", status, err)
	}
}
