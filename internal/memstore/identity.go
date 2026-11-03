package memstore

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

	"github.com/edotau/claude-code/internal/paths"
)

// Identity is a cwd resolved to the canonical repository it belongs to; worktrees and subdirs share one.
type Identity struct {
	Origin   string // canonical main checkout ("" when HomeBank)
	Base     string // filepath.Base(Origin)
	HomeBank bool   // non-repo cwd → memory/home/ (home tier)
	Harness  bool   // the config checkout itself → Key HarnessStoreKey()
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// Resolve maps root to its canonical identity; any git failure is the home tier, never an error.
func Resolve(root string) Identity {
	root = filepath.Clean(root)
	if paths.SamePath(root, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(root), paths.ConfigDir()) {
		return harnessIdentity()
	}
	if id, ok := storeIdentity(root); ok {
		return id
	}
	origin, ok := identities.Load(root)
	if !ok {
		o := MainCheckout(root)
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
	canon, store := ResolvePath(root), ResolvePath(StoreDir())
	if !within(canon, store) {
		return Identity{}, false
	}
	if within(canon, ResolvePath(HarnessStoreDir())) {
		return harnessIdentity(), true
	}
	entries, err := RepoDirs()
	if err != nil {
		return Identity{HomeBank: true}, true // never memoized by Resolve, so a recovered vault re-resolves
	}
	for _, e := range entries {
		if o := e.Origin.Origin; o != "" && within(canon, ResolvePath(e.Dir)) {
			if paths.SamePath(o, paths.ConfigDir()) {
				return harnessIdentity(), true
			}
			return Identity{Origin: o, Base: filepath.Base(o)}, true
		}
	}
	return Identity{HomeBank: true}, true
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// workspaceOrigin is the pinned non-git workspace (a store whose origin names root or an ancestor of it), or "".
func workspaceOrigin(root string) (string, error) {
	canon := ResolvePath(root)
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
	o := ResolvePath(paths.ConfigDir())
	return Identity{Origin: o, Base: filepath.Base(o), Harness: true}
}

// HarnessStoreKey is the store key for paths.ConfigDir() itself: the same origin-remote rule as any
// other repo, else its basename — the harness is not special-cased for its key (spec §2).
func HarnessStoreKey() string {
	cfg := paths.ConfigDir()
	if rb := repoKeyFromRemote(originRemoteURL(cfg)); rb != "" {
		return rb
	}
	return filepath.Base(cfg)
}

// MainCheckout is root's canonical main checkout via git (a linked worktree's common dir is <main>/.git),
// or "" outside a work tree.
func MainCheckout(root string) string {
	out, err := GitOut(root, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] == "" {
		return ""
	}
	top, common := filepath.Clean(lines[0]), filepath.Clean(lines[1])
	if filepath.Base(common) == ".git" {
		return ResolvePath(filepath.Dir(common))
	}
	return ResolvePath(top)
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
		return HarnessStoreDir(), nil
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
func FindEntry(entries []RepoEntry, origin string) *RepoEntry {
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
func takenKeys(entries []RepoEntry) func(string) (Origin, bool) {
	return func(key string) (Origin, bool) {
		if key == HarnessStoreKey() || StoreReservedDirs[key] {
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
	out, err := GitOut(checkout, "remote", "get-url", "origin")
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

// AddOriginAlias records origin (an existing checkout) as an alias of the store at dir; the only path
// that rewrites an origin record, and only with the same Key+Origin.
func AddOriginAlias(dir, origin string) error {
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
			return errors.Join(ErrOriginConflict, errors.New(origin+" already belongs to "+e.Dir))
		}
		cur.Aliases = append(slices.Clone(cur.Aliases), origin)
		return FoldOrigin(dir, cur, "")
	})
}

// RepoEntry is one repo store under StoreDir() with its validated identity.
type RepoEntry struct {
	Key, Dir string
	Origin   Origin
}

// RepoDirs lists the repo stores: real directories with a valid origin record, reserved names and symlinks skipped.
func RepoDirs() ([]RepoEntry, error) {
	ents, err := os.ReadDir(StoreDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range ents {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && !StoreReservedDirs[e.Name()] {
			keys = append(keys, e.Name())
		}
	}
	found, _, err := resolveOrigins(keys)
	if err != nil && len(found) < len(keys) {
		return nil, err // unreadable vault with unresolved stores: callers must not mint a duplicate
	}
	var out []RepoEntry
	for _, k := range keys {
		if o, ok := found[k]; ok {
			out = append(out, RepoEntry{Key: k, Dir: filepath.Join(StoreDir(), k), Origin: o})
		}
	}
	slices.SortFunc(out, func(a, b RepoEntry) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}
