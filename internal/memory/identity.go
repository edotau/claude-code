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

	"github.com/edotau/claude-code/internal/paths"
)

// Identity is a cwd resolved to the canonical repository it belongs to; worktrees and subdirs share one.
type Identity struct {
	Origin  string // canonical main checkout ("" when Global)
	Base    string // filepath.Base(Origin)
	Global  bool   // non-repo cwd → the store root (global tier)
	Harness bool   // the config checkout itself → Key harnessStoreKey()
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// Resolve maps root to its canonical identity; any git failure is the global tier, never an error.
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
		origin = mainCheckout(root)
		if origin == "" {
			origin = workspaceOrigin(root)
		}
		identities.Store(root, origin)
	}
	o := origin.(string)
	switch {
	case o == "":
		return Identity{Global: true}
	case paths.SamePath(o, paths.ConfigDir()):
		return harnessIdentity()
	}
	return Identity{Origin: o, Base: filepath.Base(o)}
}

// storeIdentity maps a cwd inside the store to the repo a store dir describes, the store top to Global; else ok=false.
// Without it the store's own git repo resolves as a project named "memory".
func storeIdentity(root string) (Identity, bool) {
	canon, store := resolvePath(root), resolvePath(StoreDir())
	if !within(canon, store) {
		return Identity{}, false
	}
	if within(canon, resolvePath(HarnessStoreDir())) {
		return harnessIdentity(), true
	}
	entries, _ := RepoDirs()
	for _, e := range entries {
		if o := e.Origin.Origin; o != "" && within(canon, resolvePath(e.Dir)) {
			if paths.SamePath(o, paths.ConfigDir()) {
				return harnessIdentity(), true
			}
			return Identity{Origin: o, Base: filepath.Base(o)}, true
		}
	}
	return Identity{Global: true}, true
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// workspaceOrigin is the pinned non-git workspace (a store whose .origin names root or an ancestor of it), or "".
func workspaceOrigin(root string) string {
	canon := resolvePath(root)
	entries, err := RepoDirs()
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		for _, o := range append([]string{e.Origin.Origin}, e.Origin.Aliases...) {
			if o != "" && len(o) > len(best) && (paths.SamePath(o, canon) || strings.HasPrefix(canon, o+string(filepath.Separator))) {
				best = e.Origin.Origin
			}
		}
	}
	return best
}

func harnessIdentity() Identity {
	o := resolvePath(paths.ConfigDir())
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

// mainCheckout is root's canonical main checkout via git (a linked worktree's common dir is <main>/.git),
// or "" outside a work tree.
func mainCheckout(root string) string {
	out, err := gitOut(root, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] == "" {
		return ""
	}
	top, common := filepath.Clean(lines[0]), filepath.Clean(lines[1])
	if filepath.Base(common) == ".git" {
		return resolvePath(filepath.Dir(common))
	}
	return resolvePath(top)
}

// RepoDir is root's store directory, read-only: it never creates anything (EnsureRepoDir does).
func RepoDir(root string) (string, error) {
	id := Resolve(root)
	switch {
	case id.Global:
		return globalStoreDir(), nil
	case id.Harness:
		return HarnessStoreDir(), nil
	}
	entries, err := RepoDirs()
	if err != nil {
		return "", err
	}
	if e := findEntry(entries, id.Origin); e != nil {
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

// findEntry is the entry whose Origin or an explicit alias names origin, or nil.
func findEntry(entries []RepoEntry, origin string) *RepoEntry {
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
// entry (a dir without a valid .origin is never claimed — extend the hash instead of clobbering).
func takenKeys(entries []RepoEntry) func(string) (Origin, bool) {
	return func(key string) (Origin, bool) {
		if key == harnessStoreKey() || StoreReservedDirs[key] {
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

// originRemoteURL is checkout's configured `origin` remote, "" when there is none (or checkout is not a repo).
func originRemoteURL(checkout string) string {
	out, err := gitOut(checkout, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
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

// EnsureRepoDir is RepoDir plus creation: the directory and its .origin exist on return, under the guarded bank lock.
func EnsureRepoDir(root string) (string, error) {
	var dir string
	err := WithWritableBankLock(BankLockWait, func() error {
		id := Resolve(root)
		var err error
		if dir, err = RepoDir(root); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if id.Global {
			return nil
		}
		// An existing record that already accepts this checkout (origin or alias) is kept, never rewritten.
		if cur, err := ReadOrigin(dir); err == nil && cur.Accepts(id.Origin) {
			return nil
		}
		return WriteOrigin(dir, Origin{Version: OriginVersion, Key: filepath.Base(dir), Origin: id.Origin})
	})
	return dir, err
}

// AddOriginAlias records origin (an existing checkout) as an alias of the store at dir; the only path
// that rewrites a .origin, and only with the same Key+Origin.
func AddOriginAlias(dir, origin string) error {
	return WithWritableBankLock(BankLockWait, func() error {
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
		if e := findEntry(entries, origin); e != nil && !paths.SamePath(e.Dir, dir) {
			return errors.Join(ErrOriginConflict, errors.New(origin+" already belongs to "+e.Dir))
		}
		cur.Aliases = append(slices.Clone(cur.Aliases), origin)
		return putOrigin(dir, cur)
	})
}

// RepoEntry is one repo store under StoreDir() with its validated identity.
type RepoEntry struct {
	Key, Dir string
	Origin   Origin
}

// RepoDirs lists the repo stores: real directories with a valid .origin, reserved names and symlinks skipped.
func RepoDirs() ([]RepoEntry, error) {
	ents, err := os.ReadDir(StoreDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RepoEntry
	for _, e := range ents {
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || StoreReservedDirs[e.Name()] {
			continue
		}
		dir := filepath.Join(StoreDir(), e.Name())
		o, err := ReadOrigin(dir)
		if err != nil {
			continue // no valid .origin: not a repo store
		}
		out = append(out, RepoEntry{Key: e.Name(), Dir: dir, Origin: o})
	}
	slices.SortFunc(out, func(a, b RepoEntry) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}
