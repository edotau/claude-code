package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s, b := StoreDir(), HomeBank()
	for got, want := range map[string]string{
		b.Path("activeContext.md"):       "activeContext.md",
		b.Sub("reports", "x.md"):         "reports/x.md",
		b.Sub("waves", "run/state.json"): "waves/run/state.json",
		b.NativeIndex():                  "MEMORY.md",
	} {
		if got != filepath.Join(s, want) {
			t.Errorf("got %s, want %s", got, filepath.Join(s, want))
		}
	}
	if got := b.SubDir("reports"); got != filepath.Join(s, "reports") {
		t.Errorf("SubDir(reports) = %s", got)
	}
}

func TestBankForIsOneBankForEveryRoot(t *testing.T) {
	isolatedConfig(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	hosttest.In(t, repo, "remote", "add", "origin", "https://github.com/org/proj.git")
	for _, root := range []string{repo, t.TempDir(), filepath.Join(t.TempDir(), "reports"), paths.ConfigDir()} {
		if b := BankFor(root); b != HomeBank() || b.Prefix() != "" {
			t.Errorf("BankFor(%s) = %+v, want the one bank", root, b)
		}
	}
	if got := Banks(); len(got) != 1 || len(BankKeys()) != 0 {
		t.Errorf("Banks = %v, BankKeys = %v, want one bank and no keys", got, BankKeys())
	}
}

func TestStoreFlatReadsStamp(t *testing.T) {
	isolatedConfig(t)
	if storeFlat() {
		t.Fatal("unstamped store reads flat")
	}
	write(t, filepath.Join(StoreDir(), layoutFile), layoutFlat+"\n")
	forgetLayout()
	if !storeFlat() {
		t.Fatal("stamped store reads legacy")
	}
}

func TestWritersRefuseLegacyStoreAndStampFreshOne(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(StoreDir(), layoutFile), layoutPerKey+"\n")
	forgetLayout()
	root := gitRepo(t, t.TempDir(), "proj")
	if _, err := EnsureBank(root, ""); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("EnsureBank = %v, want errStoreNotFlat", err)
	}
	if _, err := Init(root); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("Init = %v, want errStoreNotFlat", err)
	}
	if err := withFlatLock(BankLockWait, func() error { return nil }); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("withFlatLock = %v, want errStoreNotFlat", err)
	}
	if err := os.Remove(filepath.Join(StoreDir(), layoutFile)); err != nil {
		t.Fatal(err)
	}
	forgetLayout()
	if _, err := Init(root); err != nil || !storeFlat() {
		t.Errorf("Init on a fresh store = %v, flat=%v, want it stamped", err, storeFlat())
	}
}
