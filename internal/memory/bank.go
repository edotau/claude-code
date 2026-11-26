package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// layoutFile stamps the store root with layoutFlat, the one-bank layout.
const layoutFile = ".layout"

var layoutFlat = "flat " + strconv.Itoa(storeVersion)

// Path is one of the bank's top-level files.
func Path(name string) string { return filepath.Join(paths.MemoryStoreDir(), name) }

// NativeIndex is the one shared MEMORY.md.
func NativeIndex() string { return filepath.Join(paths.MemoryStoreDir(), "MEMORY.md") }

func Ensure(sub string) error {
	if err := stampLayout(); err != nil {
		return err
	}
	return os.MkdirAll(paths.MemorySubDir(sub), 0o755)
}

// stampLayout refuses a store stamped newer than this binary, else writes the stamp (and the store dir) when absent.
func stampLayout() error {
	p := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	b, err := os.ReadFile(p)
	if err == nil {
		stamp := strings.TrimSpace(string(b))
		n, ok := strings.CutPrefix(stamp, "flat ")
		v, aerr := strconv.Atoi(strings.TrimSpace(n))
		switch {
		case stamp == "":
			return nil
		case !ok || aerr != nil || v <= 0:
			return fmt.Errorf("memory store layout %q is not a %q stamp", stamp, "flat N")
		case v > storeVersion:
			return fmt.Errorf("store is flat %d, this binary writes flat %d: install a newer claude-code", v, storeVersion)
		}
		return nil
	}
	return paths.AtomicWrite(p, []byte(layoutFlat+"\n"), 0o644)
}

const homeDir = "home"

// flatSubs derives the store folders from the asset roles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// withFlatLock is WithBankLock for writers.
func withFlatLock(wait time.Duration, fn func() error) error {
	if err := stampLayout(); err != nil {
		return err
	}
	return WithBankLock(wait, fn)
}

// EnsureBank is the one bank with the store root created.
func EnsureBank(sub string) error {
	return WithBankLock(BankLockWait, func() error { return Ensure(sub) })
}
