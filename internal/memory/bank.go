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

// Bank is the store's one bank; a handle for the store root.
type Bank struct{}

func HomeBank() Bank { return Bank{} }

// Path is one of the bank's top-level files.
func (b Bank) Path(name string) string { return filepath.Join(StoreDir(), name) }

// SubDir is the store root: a named sub is a filename prefix, not a directory.
func (b Bank) SubDir(sub string) string { return filepath.Join(StoreDir(), sub) }

// Sub is name inside the store folder sub (archive, handoffs, reports, …); sub "" is a top-level file.
func (b Bank) Sub(sub, name string) string {
	return filepath.Join(StoreDir(), sub, filepath.FromSlash(name))
}

// NativeIndex is the one shared MEMORY.md.
func (b Bank) NativeIndex() string { return filepath.Join(StoreDir(), "MEMORY.md") }

func (b Bank) Ensure(sub string) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return os.MkdirAll(b.SubDir(sub), 0o755)
}

// layoutCache memoizes the stamp per store root: tests swap the config dir mid-process.
var layoutCache sync.Map

func storeFlat() bool {
	d := StoreDir()
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
	b, _ := os.ReadFile(filepath.Join(StoreDir(), layoutFile))
	return strings.TrimSpace(string(b)) == layoutPerKey
}

// forgetLayout drops the memoized stamp for the current store.
func forgetLayout() { layoutCache.Delete(StoreDir()) }

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
	err := paths.AtomicWrite(filepath.Join(StoreDir(), layoutFile), []byte(layoutFlat+"\n"), 0o644)
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
	ents, err := os.ReadDir(StoreDir())
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
	return !slices.Contains(flatSubs, e.Name())
}

// withFlatLock is WithBankLock for writers: a store without the flat stamp is refused.
func withFlatLock(wait time.Duration, fn func() error) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return WithBankLock(wait, fn)
}

// EnsureBank is the one bank with the store root created.
func EnsureBank(sub string) (b Bank, err error) {
	err = WithBankLock(BankLockWait, func() error {
		b = HomeBank()
		return b.Ensure(sub)
	})
	return b, err
}
