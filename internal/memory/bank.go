package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	_, ok := parseFlat(readLayoutStamp())
	layoutCache.Store(d, ok)
	return ok
}

func readLayoutStamp() string {
	b, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), layoutFile))
	return strings.TrimSpace(string(b))
}

// parseFlat reads the N of a "flat N" stamp.
func parseFlat(stamp string) (int, bool) {
	rest, ok := strings.CutPrefix(stamp, "flat ")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	return n, err == nil && n > 0
}

// StoreVersions is the store's flat N and the code's; an unstamped store reads as the code's.
func StoreVersions() (store, code int, err error) {
	stamp := readLayoutStamp()
	if stamp == "" {
		return storeVersion, storeVersion, nil
	}
	n, ok := parseFlat(stamp)
	if !ok {
		return 0, storeVersion, fmt.Errorf("memory store layout %q is not a %q stamp", stamp, "flat N")
	}
	return n, storeVersion, nil
}

// newerStoreError refuses a store written by a newer binary.
func newerStoreError(store, code int) error {
	return fmt.Errorf("store is flat %d, this binary writes flat %d: install a newer claude-code", store, code)
}

// olderStoreNote is the one-line notice for a store behind the binary.
func olderStoreNote(store, code int) string {
	return fmt.Sprintf("memory store is flat %d, this binary writes flat %d: run `claude-code memory upgrade`", store, code)
}

// noticed holds the store roots whose older-store notice already printed this process.
var noticed sync.Map

// forgetLayout drops the memoized stamp for the current store.
func forgetLayout() { layoutCache.Delete(paths.MemoryStoreDir()) }

const homeDir = "home"

// flatSubs derives the store folders from the asset roles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// requireFlat stamps a store with no layout stamp; a newer store is refused, an older one works with a notice.
func requireFlat() error {
	store, code, err := StoreVersions()
	if err != nil {
		return err
	}
	if store > code {
		return newerStoreError(store, code)
	}
	if store < code {
		if _, seen := noticed.LoadOrStore(paths.MemoryStoreDir(), true); !seen {
			fmt.Fprintln(os.Stderr, olderStoreNote(store, code))
		}
		return nil
	}
	if storeFlat() {
		return nil
	}
	err = paths.AtomicWrite(filepath.Join(paths.MemoryStoreDir(), layoutFile), []byte(layoutFlat+"\n"), 0o644)
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
