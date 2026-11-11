package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	failed, err := batchLocked("delete", []Change{{Rel: "r/fact.md", Delete: true}})
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
	assertDeletionRecovered(t, failed.ID)
}

func assertDeletionRecovered(t *testing.T, id string) {
	t.Helper()
	if len(journals(t)) != 0 {
		t.Fatal("recovery left a journal behind")
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), tombstoneDir, id)); !os.IsNotExist(err) {
		t.Fatalf("a replayed batch tombstoned its own deletion: %v", err)
	}
	if rec := readStore(t, "receipts/"+id+".json"); strings.Contains(rec, "tombstones") {
		t.Fatalf("replayed receipt lists tombstones:\n%s", rec)
	}
	if out, err := StoreRepo().run("ls-tree", "--name-only", "HEAD", "--", "r/fact.md"); err != nil || out != "" {
		t.Fatalf("recovered deletion still present at HEAD: %q, %v", out, err)
	}
	if status, err := StoreRepo().Status(); err != nil || len(status) != 0 {
		t.Fatalf("recovered store is dirty: %+v, %v", status, err)
	}
}

// A tracked fact deleted outside any batch keeps its last committed bytes under the next batch's tombstone dir.
func TestExternalDeletionIsTombstonedByTheNextBatch(t *testing.T) {
	store := seedRecoveryFact(t)
	if err := os.Remove(filepath.Join(store, "r", "fact.md")); err != nil {
		t.Fatal(err)
	}
	rec, err := batchLocked("next", []Change{{Rel: "r/next.md", Data: []byte("next\n")}})
	if err != nil || len(rec.Tombstones) != 1 || rec.Tombstones[0] != "r/fact.md" {
		t.Fatalf("next batch = %+v, %v; want r/fact.md tombstoned", rec, err)
	}
	if got := readStore(t, tombstoneDir+"/"+rec.ID+"/r/fact.md"); got != "preserve me\n" {
		t.Fatalf("tombstone copy = %q", got)
	}
}

func TestCaptureNativeRejectsFatalBlobRead(t *testing.T) {
	store := seedRecoveryFact(t)
	repo := StoreRepo()
	blob, err := repo.run("rev-parse", "HEAD:r/fact.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store, "r", "fact.md")); err != nil {
		t.Fatal(err)
	}
	_, readErr := headBytes(repo, "r/fact.md")
	if exitCode(readErr) != 128 {
		t.Fatalf("fixture must produce fatal git exit 128: %v", readErr)
	}
	j := journal{ID: "fatal-blob", Subject: "must preserve deleted fact"}
	var rec Receipt
	err = j.captureNative(repo, &rec, nil, []string{"r/fact.md"})
	if err == nil || !strings.Contains(err.Error(), "recovery copy of r/fact.md") {
		t.Fatalf("fatal blob read must abort recovery copy: %v", err)
	}
	if len(rec.Tombstones) != 0 {
		t.Fatalf("fatal read was treated as absent at HEAD: %+v", rec)
	}
}
