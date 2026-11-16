package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// layoutFile stamps the store root once migrated; absent means the per-repo layout.
const layoutFile, layoutFlat = ".layout", "flat 1"

// Bank is one repo's files in the store: <Key>--<name> when flat; Key "" is the home tier (unprefixed).
type Bank struct{ Key string }

func HomeBank() Bank      { return Bank{} }
func (b Bank) Home() bool { return b.Key == "" }

// Prefix is the "<key>--" file prefix; "" for home.
func (b Bank) Prefix() string {
	if b.Key == "" {
		return ""
	}
	return b.Key + "--"
}

// Path is one of b's top-level files (typed files, native topics of a non-harness repo).
func (b Bank) Path(name string) string { return filepath.Join(StoreDir(), b.Prefix()+name) }

func (b Bank) SubDir(sub string) string { return filepath.Join(StoreDir(), sub) }

// Sub is name, prefixed with b's key, under b's sub dir.
func (b Bank) Sub(sub, name string) string { return filepath.Join(b.SubDir(sub), b.Prefix()+name) }

// NativeIndex is the one shared MEMORY.md.
func (b Bank) NativeIndex() string { return filepath.Join(StoreDir(), "MEMORY.md") }

func (b Bank) Ensure(sub string) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return os.MkdirAll(b.SubDir(sub), 0o755)
}

// BankFor is root's bank: home for non-repo cwds, else the remote basename (else dir basename) slug.
func BankFor(root string) Bank {
	id := Resolve(root)
	switch {
	case id.HomeBank:
		return HomeBank()
	case id.Harness:
		return Bank{Key: harnessStoreKey()}
	}
	base, origin := id.Base, bankOrigin(id)
	if rb := repoKeyFromRemote(originRemoteURL(id.Origin)); rb != "" {
		base = rb
	}
	key := dashRun.ReplaceAllString(paths.Slug(base, "._-", 0, "repo"), "-")
	if o, ok := readOrigins()[key]; ok && o != origin {
		sum := sha256.Sum256([]byte(origin))
		key += "-" + hex.EncodeToString(sum[:4])
	}
	return Bank{Key: key}
}

// dashRun collapses "--" runs so no bank key contains the key/name separator.
var dashRun = regexp.MustCompile(`-{2,}`)

// originsFile maps each flat bank key to the origin that first claimed it, one "<key>\t<origin>" line each.
const originsFile = ".origins"

func readOrigins() map[string]string {
	out := map[string]string{}
	data, _ := os.ReadFile(filepath.Join(StoreDir(), originsFile))
	for l := range strings.SplitSeq(string(data), "\n") {
		if k, o, ok := strings.Cut(l, "\t"); ok {
			out[k] = o
		}
	}
	return out
}

// recordOrigin registers a new flat key's origin; the caller holds the bank lock.
func recordOrigin(key, origin string) error {
	cur := readOrigins()
	if _, ok := cur[key]; ok || origin == "" {
		return nil
	}
	cur[key] = origin
	var sb strings.Builder
	for _, k := range slices.Sorted(maps.Keys(cur)) {
		sb.WriteString(k + "\t" + cur[k] + "\n")
	}
	return paths.AtomicWrite(filepath.Join(StoreDir(), originsFile), []byte(sb.String()), 0o600)
}

// bankOrigin is the origin a flat key is claimed by: the remote URL, else the canonical checkout path.
func bankOrigin(id Identity) string {
	if url := strings.TrimSpace(originRemoteURL(id.Origin)); repoKeyFromRemote(url) != "" {
		return url
	}
	return id.Origin
}

// Banks lists home first, then every repo bank sorted by key.
func Banks() []Bank {
	out := []Bank{HomeBank()}
	for _, k := range BankKeys() {
		out = append(out, Bank{Key: k})
	}
	return out
}

// BankKeys lists the repo bank keys (home excluded), sorted.
func BankKeys() []string {
	keys := []string{}
	ents, _ := os.ReadDir(StoreDir())
	for _, e := range ents {
		if k, name, ok := strings.Cut(e.Name(), "--"); ok && k != "" && !e.IsDir() && slices.Contains(bankFiles, name) {
			keys = append(keys, k)
		}
	}
	for k := range readOrigins() { // a bank with only reports or handoffs has no typed file yet
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// BankOfFile is the bank a store file belongs to by its longest known key prefix; unknown or none is home.
func BankOfFile(path string) Bank {
	name := filepath.ToSlash(path)
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(StoreDir(), path)
		if err != nil || !filepath.IsLocal(rel) {
			return HomeBank()
		}
		name = filepath.ToSlash(rel)
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && slices.Contains(flatSubs, parts[0]) {
		name = parts[1]
	} else {
		name = parts[0]
	}
	return Bank{Key: ownerKey(name, BankKeys())}
}

// layoutCache memoizes the stamp per store root: tests swap the config dir mid-process.
var layoutCache sync.Map

func storeFlat() bool {
	d := StoreDir()
	if v, ok := layoutCache.Load(d); ok {
		return v.(bool)
	}
	b, _ := os.ReadFile(filepath.Join(d, layoutFile))
	flat := strings.TrimSpace(string(b)) == layoutFlat
	layoutCache.Store(d, flat)
	return flat
}

// forgetLayout drops the memoized stamp for the current store.
func forgetLayout() { layoutCache.Delete(StoreDir()) }

const homeDir = "home"

// flatSubs are the shared store subdirs whose first path element carries the bank prefix when flat.
var flatSubs = []string{"archive", "handoffs", "reports", "knowledge", "worktrees", "waves"}

// errStoreNotFlat is what a writer returns on a store without the flat-layout stamp.
var errStoreNotFlat = errors.New("memory store uses the retired per-repo layout; migrate it with a pre-removal claude-code binary")

// requireFlat is nil on a stamped store and stamps one with no per-repo dirs; any other gets errStoreNotFlat.
func requireFlat() error {
	if storeFlat() {
		return nil
	}
	ents, err := os.ReadDir(StoreDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // an unreadable store must not look empty and get stamped
	}
	if slices.ContainsFunc(ents, legacyBankDir) {
		return errStoreNotFlat
	}
	err = paths.AtomicWrite(filepath.Join(StoreDir(), layoutFile), []byte(layoutFlat+"\n"), 0o644)
	forgetLayout()
	return err
}

// legacyBankDir is a per-repo bank dir: any other dir, or a shared-sub name holding a typed bank file.
func legacyBankDir(e os.DirEntry) bool {
	if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
		return false
	}
	if !slices.Contains(flatSubs, e.Name()) {
		return true
	}
	return slices.ContainsFunc(bankFiles, func(f string) bool {
		_, err := os.Stat(filepath.Join(StoreDir(), e.Name(), f))
		return err == nil
	})
}

// withFlatLock is WithBankLock for writers: a store without the flat stamp is refused.
func withFlatLock(wait time.Duration, fn func() error) error {
	if err := requireFlat(); err != nil {
		return err
	}
	return WithBankLock(wait, fn)
}

// EnsureBank is root's bank with sub created, its origin recorded so the key is stable.
func EnsureBank(root, sub string) (Bank, error) {
	b := BankFor(root)
	if err := b.Ensure(sub); err != nil || b.Home() || b.Key == harnessStoreKey() {
		return b, err
	}
	origin := bankOrigin(Resolve(root))
	return b, WithBankLock(BankLockWait, func() error { return recordOrigin(b.Key, origin) })
}
