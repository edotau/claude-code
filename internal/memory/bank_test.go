package memory

import (
	"errors"
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"path/filepath"
	"testing"
)

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s := paths.MemoryStoreDir()
	for got, want := range map[string]string{
		Path("activeContext.md"):               "activeContext.md",
		paths.MemorySub("reports", "x.md"):     "reports/x.md",
		paths.MemorySub("techdebt", "area.md"): "techdebt/area.md",
		NativeIndex():                          "MEMORY.md",
	} {
		if got != filepath.Join(s, want) {
			t.Errorf("got %s, want %s", got, filepath.Join(s, want))
		}
	}
	if got := paths.MemorySubDir("reports"); got != filepath.Join(s, "reports") {
		t.Errorf("paths.MemorySubDir(reports) = %s", got)
	}
}

func TestStoreFlatReadsStamp(t *testing.T) {
	isolatedConfig(t)
	if storeFlat() {
		t.Fatal("unstamped store reads flat")
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), layoutFile), layoutFlat+"\n")
	forgetLayout()
	if !storeFlat() {
		t.Fatal("stamped store reads legacy")
	}
}

func TestWritersRefuseLegacyStoreAndStampFreshOne(t *testing.T) {
	isolatedConfig(t)
	write(t, filepath.Join(paths.MemoryStoreDir(), layoutFile), layoutPerKey+"\n")
	forgetLayout()
	if err := EnsureBank(""); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("EnsureBank = %v, want errStoreNotFlat", err)
	}
	if _, err := Init(); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("Init = %v, want errStoreNotFlat", err)
	}
	if err := withFlatLock(BankLockWait, func() error { return nil }); !errors.Is(err, errStoreNotFlat) {
		t.Errorf("withFlatLock = %v, want errStoreNotFlat", err)
	}
	if err := os.Remove(filepath.Join(paths.MemoryStoreDir(), layoutFile)); err != nil {
		t.Fatal(err)
	}
	forgetLayout()
	if _, err := Init(); err != nil || !storeFlat() {
		t.Errorf("Init on a fresh store = %v, flat=%v, want it stamped", err, storeFlat())
	}
}
