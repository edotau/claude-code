package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
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
func Resolve(root string) Identity { return resolveWith(root, gitMain) }

// gitMain is root's main checkout per git, "" outside a repo; a caller that already ran it passes the answer on.
func gitMain(root string) string { return paths.MainCheckout(root, gitOut) }

// resolveWith is Resolve with main supplying root's git main checkout on a memo miss.
func resolveWith(root string, main func(string) string) Identity {
	root = filepath.Clean(root)
	if paths.SamePath(root, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(root), paths.ConfigDir()) {
		return harnessIdentity()
	}
	if id, ok := fixedIdentity(root); ok {
		return id
	}
	origin, ok := identities.Load(root)
	if !ok {
		o := main(root)
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
func RepoDir(root string) (string, error) { return repoDirWith(root, gitMain) }

// repoDirWith is RepoDir with main supplying root's git main checkout (see resolveWith).
func repoDirWith(root string, main func(string) string) (string, error) {
	entries, err := RepoDirs()
	if err != nil {
		return "", err
	}
	id := resolveWith(root, main)
	switch {
	case id.HomeBank:
		return HomeBankDir(), nil
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
	stamp, stable := catalogStamp(keys)
	if out, ok := repoDirsMemo.load(stamp); ok {
		return out, nil
	}
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
	if stable && err == nil {
		repoDirsMemo.store(stamp, out)
	}
	return out, nil
}

// catalogMemo is RepoDirs' last answer under the stamp of what it read, so one command resolves the catalog once.
type catalogMemo struct {
	mu    sync.Mutex
	stamp string
	out   []repoEntry
}

var repoDirsMemo catalogMemo

func (m *catalogMemo) load(stamp string) ([]repoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stamp == "" || m.stamp != stamp {
		return nil, false
	}
	return slices.Clone(m.out), true
}

func (m *catalogMemo) store(stamp string, out []repoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stamp, m.out = stamp, slices.Clone(out)
}

// reset drops the memo; tests call it after rewriting the vault or catalog by hand.
func (m *catalogMemo) reset() { m.store("", nil) }

// catalogStamp names RepoDirs' inputs: the store keys plus the vault's and the catalog's size and mtime.
// stable=false while either file changed within racyWindow, where an mtime may not yet show the next write.
func catalogStamp(keys []string) (stamp string, stable bool) {
	var b strings.Builder
	b.WriteString(StoreDir() + "\x00" + strings.Join(keys, "\x00"))
	stable = true
	for _, p := range []string{vault.Path(), filepath.Join(StoreDir(), CatalogFile)} {
		fi, err := os.Stat(p)
		if err != nil {
			b.WriteString("\x00-")
			continue
		}
		fmt.Fprintf(&b, "\x00%d:%d", fi.Size(), fi.ModTime().UnixNano())
		stable = stable && time.Since(fi.ModTime()) >= racyWindow
	}
	return b.String(), stable
}

// OriginEnv carries the launching workspace's store origin to child sessions (set by agent.EnvSpec.Environ).
const OriginEnv = "HARNESS_MEMORY_ORIGIN"

// throwawayRoots are folders whose git repos are scratch or fixtures, never a project of their own.
func throwawayRoots() []string {
	roots := []string{os.TempDir(), "/tmp", filepath.Join(paths.ConfigDir(), "state")}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "worktrees", "wf-fixtures"))
	}
	return roots
}

// throwawayInTests opts a test into the rule; other tests' t.TempDir() repos live under /tmp and must stay projects.
var throwawayInTests = false

// throwaway reports whether root sits in a scratch or fixture folder, which must never mint a store.
func throwaway(root string) bool {
	if testing.Testing() && !throwawayInTests {
		return false
	}
	canon := paths.CanonDeep(root)
	for _, r := range throwawayRoots() {
		if paths.Within(canon, paths.CanonDeep(r)) {
			return true
		}
	}
	return false
}

// fixedIdentity covers roots that never get a store of their own: inside the store, or a scratch/fixture folder.
func fixedIdentity(root string) (Identity, bool) {
	if id, ok := storeIdentity(root); ok {
		return id, true
	}
	if throwaway(root) {
		return throwawayIdentity(), true
	}
	return Identity{}, false
}

// throwawayIdentity is the launching workspace from OriginEnv, else the home tier.
func throwawayIdentity() Identity {
	o := os.Getenv(OriginEnv)
	if o == "" || !filepath.IsAbs(o) {
		return Identity{HomeBank: true}
	}
	if paths.SamePath(o, paths.ConfigDir()) {
		return harnessIdentity()
	}
	o = paths.CanonDeep(o)
	return Identity{Origin: o, Base: filepath.Base(o)}
}

// LaunchOrigin is the origin a child launched from cwd inherits: OriginEnv wins; the home tier passes nothing.
func LaunchOrigin(cwd string) string {
	if o := os.Getenv(OriginEnv); o != "" {
		return o
	}
	if id := Resolve(cwd); !id.HomeBank {
		return id.Origin
	}
	return ""
}

// routedRepos are the repo stores a query names explicitly (key or origin basename as a whole word); no IDF guessing.
func routedRepos(query string) []repoEntry {
	entries, err := RepoDirs()
	if err != nil {
		return nil
	}
	q := strings.ToLower(query)
	var out []repoEntry
	for _, e := range entries {
		if routeMatch(q, e.Key) || routeMatch(q, filepath.Base(e.Origin.Origin)) {
			out = append(out, e)
		}
	}
	return out
}

// routeMatch reports whether query names raw as a whole word, with or without its leading dot.
func routeMatch(query, raw string) bool {
	name := strings.ToLower(strings.TrimPrefix(raw, "."))
	if len(name) < 4 {
		return false
	}
	return routeWord(query, strings.ToLower(raw)) || routeWord(query, name)
}

// RepoByName is the store whose key equals name, else the store whose origin basename does, for `--repo <name>`.
func RepoByName(name string) (repoEntry, bool) {
	entries, _ := RepoDirs()
	for _, e := range entries {
		if strings.EqualFold(e.Key, name) {
			return e, true
		}
	}
	for _, e := range entries {
		if strings.EqualFold(filepath.Base(e.Origin.Origin), name) {
			return e, true
		}
	}
	return repoEntry{}, false
}

// routeWord uses the same separator set at both ends, so dotted names stay whole.
func routeWord(query, name string) bool {
	for offset := 0; offset <= len(query)-len(name); {
		i := strings.Index(query[offset:], name)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(name)
		if (i == 0 || !routeWordByte(query[i-1])) && (end == len(query) || !routeWordByte(query[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

// routeWordByte reports whether c continues a route word.
func routeWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-'
}

// RepoRoot resolves --repo like recall: a catalog store name maps to its origin checkout, else a path.
func RepoRoot(repo string) string {
	if repo == "" {
		return Root(repo)
	}
	if e, ok := RepoByName(repo); ok && e.Origin.Origin != "" {
		return e.Origin.Origin
	}
	return Root(repo)
}

// ResolveRepo is RepoRoot that refuses a guess: "" is the current repo, else a catalog store name or an existing path.
func ResolveRepo(repo string) (string, error) {
	if repo == "" {
		return Root(""), nil
	}
	if e, ok := RepoByName(repo); ok && e.Origin.Origin != "" {
		return e.Origin.Origin, nil
	}
	if _, err := os.Stat(repo); err == nil {
		return Root(repo), nil
	}
	return "", fmt.Errorf("--repo %q is neither an existing path nor a known store (known stores: %s)",
		repo, strings.Join(RepoKeys(), ", "))
}

// RepoKeys lists the catalog's repo keys (dirs under memory/ with a valid origin record).
func RepoKeys() []string {
	ents, err := RepoDirs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Key)
	}
	return out
}
