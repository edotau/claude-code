//go:build unix

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBankLockPathLocation pins the physical lock file and guards against an accidental ".lock.lock" variant.
func TestBankLockPathLocation(t *testing.T) {
	cfg := tempConfig(t)
	p := BankLockPath()
	want := filepath.Join(cfg, "state", "harness", "memory", "bank.lock")
	if p != want {
		t.Fatalf("BankLockPath() = %q, want %q", p, want)
	}
	if strings.HasSuffix(p, ".lock.lock") {
		t.Fatalf("BankLockPath() produced a .lock.lock variant: %q", p)
	}
	if err := WithBankLock(10*time.Millisecond, func() error { return nil }); err != nil {
		t.Fatalf("WithBankLock: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); err == nil {
		t.Fatalf("a %s.lock file must never appear", p)
	}
}

// TestWithBankLockContention mirrors paths.TestTryFileLockContention: another fd holding the flock must
// block WithBankLock out with ErrBankBusy and never run fn, then let it through once released.
func TestWithBankLockContention(t *testing.T) {
	tempConfig(t)
	path := BankLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	ran := false
	err = WithBankLock(10*time.Millisecond, func() error { ran = true; return nil })
	if err != ErrBankBusy {
		t.Fatalf("WithBankLock while held = %v, want ErrBankBusy", err)
	}
	if ran {
		t.Fatal("fn must not run while the bank lock is held elsewhere")
	}

	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()

	ran = false
	if err := WithBankLock(10*time.Millisecond, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("WithBankLock after release: %v", err)
	}
	if !ran {
		t.Fatal("fn must run once the bank lock is released")
	}
}

// TestSettleBankLockLivesUnderStateNotStore: SettleBank's physical lock is BankLockPath() under state
// — never a dotfile beside the committed bank files.
func TestSettleBankLockLivesUnderStateNotStore(t *testing.T) {
	cfg := tempConfig(t)
	root := repoRoot(t, "proj")
	dir := Dir(root)
	if _, err := SettleBank(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	want := BankLockPath()
	if !strings.HasPrefix(want, filepath.Join(cfg, "state")) {
		t.Fatalf("bank lock %s is not under state", want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("lock file missing at %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate.lock")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
}

// TestPromoteLockLivesUnderGlobalLocksDir: PromoteLocalToGlobal's lock is state/harness/memory/locks/home/.promote.lock.
func TestPromoteLockLivesUnderGlobalLocksDir(t *testing.T) {
	cfg := tempConfig(t)
	want := filepath.Join(cfg, "state", "harness", "memory", "locks", "home", ".promote.lock")
	if got := homeBankLockPath(); got != want {
		t.Fatalf("homeBankLockPath() = %q, want %q", got, want)
	}
	root := repoRoot(t, "proj")
	write(t, filepath.Join(Dir(root), "conventions.md"), strings.Repeat("- rule\n", 40))
	if _, err := PromoteLocalToGlobal(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("promote lock file missing at %s: %v", want, err)
	}
}

func TestMaintenanceMarker(t *testing.T) {
	tempConfig(t)
	if _, ok := InMaintenance(); ok {
		t.Fatal("fresh config in maintenance")
	}
	if err := ClearMaintenance(); err != nil {
		t.Fatalf("clearing an absent marker: %v", err)
	}
	if err := SetMaintenance("migrate apply"); err != nil {
		t.Fatal(err)
	}
	if reason, ok := InMaintenance(); !ok || reason != "migrate apply" {
		t.Fatalf("InMaintenance = %q, %v", reason, ok)
	}
	if MaintenancePath() != filepath.Join(StateDir(), "maintenance") {
		t.Fatal("MaintenancePath moved")
	}
	if err := ClearMaintenance(); err != nil {
		t.Fatal(err)
	}
	if _, ok := InMaintenance(); ok {
		t.Fatal("marker survived Clear")
	}
}

func TestStorePathsAndInsideStore(t *testing.T) {
	cfg := tempConfig(t)
	// No origin remote here, so the harness key falls back to the config dir's own basename, not ".claude".
	if StoreDir() != filepath.Join(cfg, "memory") || HarnessStoreDir() != filepath.Join(cfg, "memory", filepath.Base(cfg)) {
		t.Fatal("store paths moved")
	}
	if JournalDir() != filepath.Join(StateDir(), "journal") || KnowledgeDir("/x") != "/x/knowledge" {
		t.Fatal("journal/knowledge paths moved")
	}
	if ArchiveDir("/x") != "/x/archive" || HandoffsDir("/x") != "/x/handoffs" {
		t.Fatal("archive/handoffs paths moved")
	}
	cases := map[string]struct {
		rel string
		ok  bool
	}{
		filepath.Join(StoreDir(), "repo", "MEMORY.md"): {"repo/MEMORY.md", true},
		filepath.Join(StoreDir(), ".git", "config"):    {"", false},
		filepath.Join(StoreDir(), ".gitx"):             {".gitx", true},
		filepath.Join(cfg, "memory-other", "x.md"):     {"", false},
		filepath.Join(cfg, "docs", "x.md"):             {"", false},
		StoreDir():                                     {".", true},
	}
	for p, want := range cases {
		if rel, ok := InsideStore(p); rel != want.rel || ok != want.ok {
			t.Errorf("InsideStore(%q) = %q, %v; want %q, %v", p, rel, ok, want.rel, want.ok)
		}
	}
	// A symlink escaping the store is outside it even though its lexical path is inside.
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(StoreDir(), "escape.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := InsideStore(filepath.Join(StoreDir(), "escape.md")); ok {
		t.Error("InsideStore accepted a symlink escaping the store")
	}
}

// shortBankWait shrinks BankLockWait so a contended-lock test fails fast instead of waiting the 5s default.
func shortBankWait(t *testing.T) {
	t.Helper()
	old := BankLockWait
	BankLockWait = 100 * time.Millisecond
	t.Cleanup(func() { BankLockWait = old })
}
