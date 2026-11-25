package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// layoutFile stamps the store root with layoutFlat, the one-bank layout.
const layoutFile, layoutFlat = ".layout", "flat 3"

// Path is one of the bank's top-level files.
func Path(name string) string { return filepath.Join(paths.MemoryStoreDir(), name) }

// NativeIndex is the one shared MEMORY.md.
func NativeIndex() string { return filepath.Join(paths.MemoryStoreDir(), "MEMORY.md") }

func Ensure(sub string) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return os.MkdirAll(paths.MemorySubDir(sub), 0o755)
}

// layoutCache memoizes the stamp per store root: tests swap the config dir mid-process.
var layoutCache sync.Map

func storeFlat() bool {
	d := paths.MemoryStoreDir()
	if v, ok := layoutCache.Load(d); ok {
		return v.(bool)
	}
	b, _ := os.ReadFile(filepath.Join(d, layoutFile))
	stamp := strings.TrimSpace(string(b))
	flat := stamp == layoutFlat
	layoutCache.Store(d, flat)
	return flat
}

// forgetLayout drops the memoized stamp for the current store.
func forgetLayout() { layoutCache.Delete(paths.MemoryStoreDir()) }

const homeDir = "home"

// flatSubs derives the store folders from the asset roles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// requireFlat stamps a store that has no layout stamp yet; any other stamp is refused, never overwritten.
func requireFlat() error {
	if storeFlat() {
		return nil
	}
	path := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("memory store layout %q is not %q: refusing to write", strings.TrimSpace(string(b)), layoutFlat)
	}
	err := paths.AtomicWrite(path, []byte(layoutFlat+"\n"), 0o644)
	forgetLayout()
	return err
}

// withFlatLock is WithBankLock for writers, stamping a fresh store first.
func withFlatLock(wait time.Duration, fn func() error) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return WithBankLock(wait, fn)
}

// EnsureBank is the one bank with the store root created.
func EnsureBank(sub string) error {
	return WithBankLock(BankLockWait, func() error { return Ensure(sub) })
}
