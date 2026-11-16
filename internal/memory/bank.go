package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/paths"
)

// layoutFile stamps the store root once migrated; absent means the per-repo layout.
const layoutFile, layoutFlat = ".layout", "flat 1"

// Bank is one repo's files in the store: <Key>--<name> when flat; Key "" is the home tier (unprefixed).
type Bank struct{ Key string }

func HomeBank() Bank      { return Bank{} }
func (b Bank) Home() bool { return b.Key == "" }

// Prefix is the "<key>--" file prefix; "" for home and for the legacy layout, whose files are never prefixed.
func (b Bank) Prefix() string {
	if b.Key == "" || !storeFlat() {
		return ""
	}
	return b.Key + "--"
}

// Path is one of b's top-level files (typed files, native topics of a non-harness repo).
func (b Bank) Path(name string) string {
	if !storeFlat() {
		return filepath.Join(b.legacyDir(), name)
	}
	return filepath.Join(StoreDir(), b.Prefix()+name)
}

func (b Bank) SubDir(sub string) string {
	if !storeFlat() {
		return filepath.Join(b.legacyDir(), sub)
	}
	return filepath.Join(StoreDir(), sub)
}

// Sub is name under b's sub dir; name is prefixed only when flat.
func (b Bank) Sub(sub, name string) string {
	if !storeFlat() {
		return filepath.Join(b.SubDir(sub), name)
	}
	return filepath.Join(b.SubDir(sub), b.Prefix()+name)
}

// NativeIndex is b's MEMORY.md: the one shared index when flat.
func (b Bank) NativeIndex() string {
	if !storeFlat() {
		return filepath.Join(b.legacyDir(), "MEMORY.md")
	}
	return filepath.Join(StoreDir(), "MEMORY.md")
}

func (b Bank) Ensure(sub string) error { return os.MkdirAll(b.SubDir(sub), 0o755) }

func (b Bank) legacyDir() string {
	if b.Key == "" {
		return HomeBankDir()
	}
	return filepath.Join(StoreDir(), b.Key)
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
	if !storeFlat() {
		if d, err := RepoDir(root); err == nil {
			return Bank{Key: filepath.Base(d)}
		}
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
	if storeFlat() {
		ents, _ := os.ReadDir(StoreDir())
		for _, e := range ents {
			if k, name, ok := strings.Cut(e.Name(), "--"); ok && k != "" && !e.IsDir() && slices.Contains(bankFiles, name) {
				keys = append(keys, k)
			}
		}
	} else {
		keys = append(keys, harnessStoreKey())
		entries, _ := RepoDirs()
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
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

// flatForced is 0 when unforced, 1 legacy, 2 flat (setStoreFlat and the migration force it).
var flatForced atomic.Int32

func storeFlat() bool {
	if v := flatForced.Load(); v != 0 {
		return v == 2
	}
	d := StoreDir()
	if v, ok := layoutCache.Load(d); ok {
		return v.(bool)
	}
	b, _ := os.ReadFile(filepath.Join(d, layoutFile))
	flat := strings.TrimSpace(string(b)) == layoutFlat
	layoutCache.Store(d, flat)
	return flat
}

// forgetLayout drops the memoized stamp for the current store (after migrate or revert writes it).
func forgetLayout() { layoutCache.Delete(StoreDir()) }

// EnsureBank is root's bank with sub created; on the legacy layout it also records root's origin so the key is stable.
func EnsureBank(root, sub string) (Bank, error) {
	if !storeFlat() {
		if _, err := EnsureRepoDir(root); err != nil {
			return Bank{}, err
		}
	}
	b := BankFor(root)
	if err := b.Ensure(sub); err != nil || !storeFlat() || b.Home() || b.Key == harnessStoreKey() {
		return b, err
	}
	origin := bankOrigin(Resolve(root))
	return b, WithBankLock(BankLockWait, func() error { return recordOrigin(b.Key, origin) })
}
