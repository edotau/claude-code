package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// layoutFile stamps the store root: layoutFlat is the one-bank layout; layoutFlat2 is older, layoutPerKey retired.
const layoutFile, layoutFlat, layoutFlat2, layoutPerKey = ".layout", "flat 3", "flat 2", "flat 1"

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
	flat := stamp == layoutFlat || stamp == layoutFlat2
	layoutCache.Store(d, flat)
	return flat
}

func storePerKey() bool {
	b, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), layoutFile))
	return strings.TrimSpace(string(b)) == layoutPerKey
}

// forgetLayout drops the memoized stamp for the current store.
func forgetLayout() { layoutCache.Delete(paths.MemoryStoreDir()) }

const homeDir = "home"

// flatSubs are the store folders; derived from folderRoles so sandbox and import never miss one.
var flatSubs = storeFolderNames()

// errStoreNotFlat is what a writer returns on a store without the flat-layout stamp.
var errStoreNotFlat = errors.New("memory store uses the retired per-repo layout; run `claude-code memory migrate`")

// requireFlat is nil on a stamped store and stamps one with no per-repo dirs; any other gets errStoreNotFlat.
func requireFlat() error {
	if err := checkFlat(); err != nil || storeFlat() {
		return err
	}
	err := paths.AtomicWrite(filepath.Join(paths.MemoryStoreDir(), layoutFile), []byte(layoutFlat+"\n"), 0o644)
	forgetLayout()
	return err
}

// checkFlat is requireFlat without the stamp, for read-only paths: errStoreNotFlat only on a per-repo store.
func checkFlat() error {
	if storeFlat() {
		return nil
	}
	if storePerKey() {
		return errStoreNotFlat
	}
	ents, err := os.ReadDir(paths.MemoryStoreDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // an unreadable store must not look empty and get stamped
	}
	if slices.ContainsFunc(ents, legacyBankDir) {
		return errStoreNotFlat
	}
	return nil
}

// legacyBankDir is a per-repo bank dir: any dir other than a leftover shared sub (kept until the one-off move).
func legacyBankDir(e os.DirEntry) bool {
	if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
		return false
	}
	return !slices.Contains(flatSubs, e.Name()) && e.Name() != "waves" // waves/ is retired run state, left alone
}

// withFlatLock is WithBankLock for writers: a store without the flat stamp is refused.
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
