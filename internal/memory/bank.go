package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// layoutFile stamps the store root with layoutFlat, the one-bank layout.
const layoutFile = ".layout"

const storeVersion = 4 // the "flat N" this binary writes

// StoreVersion is the "flat N" this binary writes.
func StoreVersion() int { return storeVersion }

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

// stampLayout refuses a newer or malformed stamp, rewrites an older one, and writes it when absent.
func stampLayout() error {
	p := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read memory store layout: %w", err)
	}
	if err == nil {
		v, err := StoreLayoutVersion(string(b))
		if err != nil {
			return err
		}
		if v > storeVersion {
			return fmt.Errorf("store is flat %d, this binary writes flat %d: install a newer claude-code", v, storeVersion)
		}
		if v == storeVersion {
			return nil
		}
	}
	return paths.AtomicWrite(p, []byte(layoutFlat+"\n"), 0o644)
}

// StoreLayoutVersion parses a positive, unsigned flat stamp for writers and doctor.
func StoreLayoutVersion(stamp string) (int, error) {
	stamp = strings.TrimSpace(stamp)
	n, ok := strings.CutPrefix(stamp, "flat ")
	n = strings.TrimSpace(n)
	v, err := strconv.Atoi(n)
	if !ok || err != nil || v <= 0 || strings.HasPrefix(n, "+") {
		return 0, fmt.Errorf("memory store layout %q is not a %q stamp", stamp, "flat N")
	}
	return v, nil
}

const homeDir = "home"

// flatSubs derives the store folders from the asset roles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// withBankLock is WithBankLock for writers.
func withBankLock(wait time.Duration, fn func() error) error {
	return WithBankLock(wait, func() error {
		if err := stampLayout(); err != nil {
			return err
		}
		return fn()
	})
}

// EnsureBank is the one bank with the store root created.
func EnsureBank(sub string) error {
	return WithBankLock(BankLockWait, func() error { return Ensure(sub) })
}
