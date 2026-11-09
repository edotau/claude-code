package bank

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/memstore"
)

func denyBankAccess(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission denial is ineffective as root")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
}

func TestShowBlockReturnsWalkErrors(t *testing.T) {
	for _, sub := range []string{"", "unreadable"} {
		t.Run(sub, func(t *testing.T) {
			isolatedConfig1(t)
			dir := filepath.Join(memstore.StoreDir(), sub)
			write(t, filepath.Join(dir, "decisionLog.md"), "## Hidden <!-- id:deadbeef -->\nbody\n")
			denyBankAccess(t, dir)
			_, _, err := ShowBlock("deadbeef")
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("ShowBlock = %v, want walk permission error, not ErrNoBlock", err)
			}
		})
	}
}

func TestFormatBankCheckReturnsDiscoveryErrors(t *testing.T) {
	for _, sub := range []string{"stat", "", "archive"} {
		t.Run(sub, func(t *testing.T) {
			isolatedConfig1(t)
			want := fmtDiscoveryFailure(t, sub)
			_, err := FormatBank(true)
			if !errors.Is(err, want) {
				t.Fatalf("fmt --check = %v, want discovery error %v", err, want)
			}
		})
	}
}

func fmtDiscoveryFailure(t *testing.T, sub string) error {
	t.Helper()
	if sub == "stat" {
		file := filepath.Join(memstore.HomeBankDir(), "decisionLog.md")
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(file, file); err != nil {
			t.Fatal(err)
		}
		return syscall.ELOOP
	}
	dir := filepath.Join(memstore.HomeBankDir(), sub)
	write(t, filepath.Join(dir, "decisionLog.md"), "## Hidden\nbody\n")
	denyBankAccess(t, dir)
	return fs.ErrPermission
}

func TestCorpusBlocksReturnsReadErrors(t *testing.T) {
	root := isolatedConfig1(t)
	file := filepath.Join(memstore.HomeBankDir(), "decisionLog.md")
	write(t, file, "## Hidden\nbody\n")
	denyBankAccess(t, file)
	hits, err := CorpusBlocks(root)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("CorpusBlocks = %v, %v; want file permission error", hits, err)
	}
}

func TestCommitChangesReturnsJournalScanErrors(t *testing.T) {
	isolatedConfig1(t)
	dir := memstore.JournalDir()
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

func TestBankFilesReturnsIndependentSlice(t *testing.T) {
	want := append([]string(nil), bankFiles...)
	got := BankFiles()
	original := got[0]
	t.Cleanup(func() { got[0] = original })
	got[0] = "caller-owned.md"
	if next := BankFiles(); !reflect.DeepEqual(next, want) {
		t.Fatalf("caller mutation changed BankFiles: got %v, want %v", next, want)
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
	if err == nil || errors.Is(err, ErrDivergent) {
		t.Fatalf("held ref lock must fail the deletion commit: %v", err)
	}
	if len(journals(t)) != 1 {
		t.Fatal("failed deletion did not retain its journal")
	}
	staged, err := StoreRepo().run("diff", "--cached", "--name-only")
	if err != nil || !strings.Contains(staged, "receipts/"+failed.ID+".json") {
		t.Fatalf("failed deletion did not stage its receipt: %q, %v", staged, err)
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
	if got := readStore(t, tombstoneDir+"/"+id+"/r/fact.md"); got != "preserve me\n" {
		t.Fatalf("recovered tombstone = %q", got)
	}
	if out, err := StoreRepo().run("ls-tree", "--name-only", "HEAD", "--", "r/fact.md"); err != nil || out != "" {
		t.Fatalf("recovered deletion still present at HEAD: %q, %v", out, err)
	}
	if status, err := StoreRepo().Status(); err != nil || len(status) != 0 {
		t.Fatalf("recovered store is dirty: %+v, %v", status, err)
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
