package memory

import (
	"crypto/sha256"
	"encoding/hex"
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

// Identity is a cwd resolved to the canonical repository it belongs to; worktrees and subdirs share one.
type Identity struct {
	Origin   string // canonical main checkout ("" when HomeBank)
	Base     string // filepath.Base(Origin)
	HomeBank bool   // non-repo cwd → memory/home/ (home tier)
	Harness  bool   // the config checkout itself → Key harnessStoreKey()
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// Resolve maps root to its canonical identity; any git failure is the home tier, never an error.
func Resolve(root string) Identity {
	root = filepath.Clean(root)
	if paths.SamePath(root, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(root), paths.ConfigDir()) {
		return harnessIdentity()
	}
	if id, ok := fixedIdentity(root); ok {
		return id
	}
	origin, ok := identities.Load(root)
	if !ok {
		o := paths.MainCheckout(root, gitOut)
		if o != "" {
			o = paths.CanonDeep(o)
		}
		if o == "" {
			var err error
			if o, err = workspaceOrigin(root); err != nil {
				return Identity{HomeBank: true} // transient vault failure: answer now, memoize nothing
			}
		}
		origin = o
		identities.Store(root, origin)
	}
	o := origin.(string)
	switch {
	case o == "":
		return Identity{HomeBank: true}
	case paths.SamePath(o, paths.ConfigDir()):
		return harnessIdentity()
	}
	return Identity{Origin: o, Base: filepath.Base(o)}
}

// storeIdentity maps a cwd inside the store to the repo a store dir describes, the store top to HomeBank; else ok=false.
// Without it the store's own git repo resolves as a project named "memory".
func storeIdentity(root string) (Identity, bool) {
	canon, store := paths.CanonDeep(root), paths.CanonDeep(StoreDir())
	if !paths.Within(canon, store) {
		return Identity{}, false
	}
	if paths.Within(canon, paths.CanonDeep(harnessStoreDir())) {
		return harnessIdentity(), true
	}
	entries, err := RepoDirs()
	if err != nil {
		return Identity{HomeBank: true}, true // never memoized by Resolve, so a recovered vault re-resolves
	}
	for _, e := range entries {
		if o := e.Origin.Origin; o != "" && paths.Within(canon, paths.CanonDeep(e.Dir)) {
			if paths.SamePath(o, paths.ConfigDir()) {
				return harnessIdentity(), true
			}
			return Identity{Origin: o, Base: filepath.Base(o)}, true
		}
	}
	return Identity{HomeBank: true}, true
}

// workspaceOrigin is the pinned non-git workspace (a store whose origin names root or an ancestor of it), or "".
func workspaceOrigin(root string) (string, error) {
	canon := paths.CanonDeep(root)
	entries, err := RepoDirs()
	if err != nil {
		return "", err
	}
	best, bestLen := "", 0
	for _, e := range entries {
		for _, o := range append([]string{e.Origin.Origin}, e.Origin.Aliases...) {
			if o != "" && len(o) > bestLen && (paths.SamePath(o, canon) || strings.HasPrefix(canon, o+string(filepath.Separator))) {
				best, bestLen = e.Origin.Origin, len(o)
			}
		}
	}
	return best, nil
}

func harnessIdentity() Identity {
	o := paths.CanonDeep(paths.ConfigDir())
	return Identity{Origin: o, Base: filepath.Base(o), Harness: true}
}

// harnessStoreKey is the store key for paths.ConfigDir() itself: the same origin-remote rule as any
// other repo, else its basename — the harness is not special-cased for its key (spec §2).
func harnessStoreKey() string {
	cfg := paths.ConfigDir()
	if rb := repoKeyFromRemote(originRemoteURL(cfg)); rb != "" {
		return rb
	}
	return filepath.Base(cfg)
}

// RepoDir is root's store directory, read-only: it never creates anything (EnsureRepoDir does).
func RepoDir(root string) (string, error) {
	entries, err := RepoDirs()
	if err != nil {
		return "", err
	}
	id := Resolve(root)
	switch {
	case id.HomeBank:
		return homeStoreDir(), nil
	case id.Harness:
		return harnessStoreDir(), nil
	}
	if e := FindEntry(entries, id.Origin); e != nil {
		return e.Dir, nil
	}
	base := id.Base
	if rb := repoKeyFromRemote(originRemoteURL(id.Origin)); rb != "" {
		base = rb
	}
	key, err := slugKey(base, id.Origin, takenKeys(entries))
	if err != nil {
		return "", err
	}
	return filepath.Join(StoreDir(), key), nil
}

// FindEntry is the entry whose Origin or an explicit alias names origin, or nil.
func FindEntry(entries []repoEntry, origin string) *repoEntry {
	for i := range entries {
		if entries[i].Origin.Accepts(origin) {
			return &entries[i]
		}
	}
	return nil
}

// Accepts reports whether origin is o's canonical origin or one of its explicit aliases.
func (o Origin) Accepts(origin string) bool {
	if paths.SamePath(o.Origin, origin) {
		return true
	}
	for _, a := range o.Aliases {
		if paths.SamePath(a, origin) {
			return true
		}
	}
	return false
}

// takenKeys reports a key as taken when it is reserved, the harness key, a known repo, or any existing
// entry (a dir without a valid origin record is never claimed — extend the hash instead of clobbering).
func takenKeys(entries []repoEntry) func(string) (Origin, bool) {
	return func(key string) (Origin, bool) {
		if key == harnessStoreKey() || storeReservedDirs[key] {
			return Origin{}, true
		}
		for _, e := range entries {
			if e.Key == key {
				return e.Origin, true
			}
		}
		if _, err := os.Lstat(filepath.Join(StoreDir(), key)); err == nil {
			return Origin{}, true
		}
		return Origin{}, false
	}
}

// remoteCache memoizes originRemoteURL per checkout, misses included; a remote change mid-process is not seen.
var remoteCache sync.Map

// originRemoteURL is checkout's configured `origin` remote, "" when there is none (or checkout is not a repo).
func originRemoteURL(checkout string) string {
	if v, ok := remoteCache.Load(checkout); ok {
		return v.(string)
	}
	out, err := gitOut(checkout, "remote", "get-url", "origin")
	if err != nil {
		out = ""
	}
	remoteCache.Store(checkout, out)
	return out
}

// repoKeyFromRemote is the basename of a git remote URL (ssh://, git@host:, https://), trailing ".git"
// stripped (spec: store re-layout §1); "" for an empty or unparseable url.
func repoKeyFromRemote(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), "/")
	url = strings.TrimSuffix(url, ".git")
	if url == "" {
		return ""
	}
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		return url[i+1:]
	}
	return url
}

// slugKey is the directory key for origin: the basename slug, or on a collision with another origin the
// slug plus a growing SHA256 prefix of the canonical origin — never an overwrite.
func slugKey(base, origin string, taken func(key string) (Origin, bool)) (string, error) {
	slug := paths.Slug(base, "._-", 0, "repo")
	if strings.Trim(slug, "._-") == "" {
		slug = "repo"
	}
	sum := sha256.Sum256([]byte(origin))
	hash := hex.EncodeToString(sum[:])
	key := slug
	for n := 8; ; n += 4 {
		o, ok := taken(key)
		if !ok || o.Origin == origin {
			return key, nil
		}
		if n > len(hash) {
			return "", errors.New("origin key collision could not be resolved")
		}
		key = slug + "--" + hash[:n]
	}
}

// EnsureRepoDir is RepoDir plus creation: the directory and its origin record exist on return, under the bank lock.
func EnsureRepoDir(root string) (string, error) {
	var dir string
	err := WithBankLock(BankLockWait, func() error {
		var err error
		if dir, err = RepoDir(root); err != nil {
			return err
		}
		return ensureOriginDir(dir, Resolve(root))
	})
	return dir, err
}

// ensureOriginDir removes a newly created empty directory when its origin cannot be recorded.
func ensureOriginDir(dir string, id Identity) error {
	_, before := os.Lstat(dir)
	if before != nil && !errors.Is(before, fs.ErrNotExist) {
		return before
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if id.HomeBank {
		return nil
	}
	if cur, err := ReadOrigin(dir); err == nil && cur.Accepts(id.Origin) {
		return nil
	}
	err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: filepath.Base(dir), Origin: id.Origin})
	if err != nil && errors.Is(before, fs.ErrNotExist) {
		return errors.Join(err, os.Remove(dir))
	}
	return err
}

// addOriginAlias records origin (an existing checkout) as an alias of the store at dir; the only path
// that rewrites an origin record, and only with the same Key+Origin.
func addOriginAlias(dir, origin string) error {
	return WithBankLock(BankLockWait, func() error {
		origin, err := filepath.EvalSymlinks(filepath.Clean(origin))
		if err != nil {
			return err
		}
		cur, err := ReadOrigin(dir)
		if err != nil {
			return err
		}
		if cur.Accepts(origin) {
			return nil
		}
		entries, err := RepoDirs()
		if err != nil {
			return err
		}
		if e := FindEntry(entries, origin); e != nil && !paths.SamePath(e.Dir, dir) {
			return errors.Join(errOriginConflict, errors.New(origin+" already belongs to "+e.Dir))
		}
		cur.Aliases = append(slices.Clone(cur.Aliases), origin)
		return foldOrigin(dir, cur, "")
	})
}

// repoEntry is one repo store under StoreDir() with its validated identity.
type repoEntry struct {
	Key, Dir string
	Origin   Origin
}

// storeKeys names the real, non-reserved directories among ents.
func storeKeys(ents []fs.DirEntry) []string {
	var keys []string
	for _, e := range ents {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && !storeReservedDirs[e.Name()] {
			keys = append(keys, e.Name())
		}
	}
	return keys
}

// RepoDirs lists the repo stores: real directories with a valid origin record, reserved names and symlinks skipped.
func RepoDirs() ([]repoEntry, error) {
	ents, err := os.ReadDir(StoreDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keys := storeKeys(ents)
	found, _, err := resolveOrigins(keys)
	if err != nil && len(found) < len(keys) {
		return nil, err // unreadable vault with unresolved stores: callers must not mint a duplicate
	}
	var out []repoEntry
	for _, k := range keys {
		if o, ok := found[k]; ok {
			out = append(out, repoEntry{Key: k, Dir: filepath.Join(StoreDir(), k), Origin: o})
		}
	}
	slices.SortFunc(out, func(a, b repoEntry) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// bankLockFile is the ONE store-wide lock; every harness write and every memory Git operation runs under it.
const bankLockFile = "bank.lock"

// BankLockWait bounds how long a worker waits for the bank before giving up with ErrBankBusy.
var BankLockWait = 5 * time.Second

// ErrBankBusy is returned when the bank lock is not granted within the wait.
var ErrBankBusy = errors.New("memory store busy: another harness worker holds the bank lock")

// BankLockPath is the PHYSICAL lock file; nothing appends ".lock" to it (plan §3.1: no .lock.lock variants).
func BankLockPath() string { return filepath.Join(StateDir(), bankLockFile) }

// storeLockKey namespaces a bank dir's runtime lock files under state: the store dir's base name,
// or "home" for the home bank itself — locks never live beside the committed memory store.
func storeLockKey(dir string) string {
	if paths.SamePath(dir, HomeBankDir()) {
		return "home"
	}
	return filepath.Base(dir)
}

// storeLockDir is state's per-key lock directory for dir's bank, created 0700 on first use.
func storeLockDir(dir string) string {
	d := filepath.Join(StateDir(), "locks", storeLockKey(dir))
	_ = os.MkdirAll(d, 0o700)
	return d
}

// WithBankLock runs fn holding the bank lock; wait<=0 means BankLockWait. Lock order: worker claim → bank → git index.
// Not reentrant: *Locked helpers never take it, so settlement cannot deadlock on itself.
func WithBankLock(wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = BankLockWait
	}
	ran, err := paths.TryFileLock(BankLockPath(), wait, fn)
	if err == nil && !ran {
		return ErrBankBusy
	}
	return err
}
