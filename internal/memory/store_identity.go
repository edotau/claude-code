package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	origin, ok := identities.Load(root)
	if !ok {
		if id, fixed := fixedIdentity(root); fixed {
			return id
		}
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
	if cur, err := readOrigin(dir); err == nil && cur.Accepts(id.Origin) {
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
		cur, err := readOrigin(dir)
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
		if e.IsDir() && !storeReservedDirs[e.Name()] {
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

// catalogStamp names RepoDirs' inputs: the store keys plus the vault's and the catalog's size and mtime.
// stable=false while either file changed within racyWindow, where an mtime may not yet show the next write.
func catalogStamp(keys []string) (stamp string, stable bool) {
	var b strings.Builder
	b.WriteString(StoreDir() + "\x00" + strings.Join(keys, "\x00"))
	stable = true
	for _, p := range []string{vault.Path(), filepath.Join(StoreDir(), catalogFile)} {
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
		// TMPDIR=$HOME (or /) would make every repo scratch
		roots = slices.DeleteFunc(roots, func(r string) bool { return paths.SamePath(r, home) || filepath.Dir(r) == r })
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

// repoByName is the store whose key equals name, else the store whose origin basename does, for `--repo <name>`.
func repoByName(name string) (repoEntry, bool) {
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
	r, _ := lookupRepo(repo)
	return r
}

// lookupRepo is repo's root and whether it was known: "" is the current repo, else a store name or an existing path.
func lookupRepo(repo string) (string, bool) {
	if repo == "" {
		return root(""), true
	}
	if e, ok := repoByName(repo); ok && e.Origin.Origin != "" {
		return e.Origin.Origin, true
	}
	_, err := os.Stat(repo)
	return root(repo), err == nil
}

// resolveRepo is RepoRoot that refuses a guess: "" is the current repo, else a catalog store name or an existing path.
func resolveRepo(repo string) (string, error) {
	if r, ok := lookupRepo(repo); ok {
		return r, nil
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

// Migration: remove after 2026-11-17 once every machine has run a post-2026-10-20 build.
// legacyOriginFile is the pre-vault per-store record; it is ignored (neither imported nor removed) and only skipped.
const legacyOriginFile = ".origin"

// OriginVersion is the schema version readOrigin accepts.
const OriginVersion = 1

// Origin ties a store directory to the canonical checkout it serves; Aliases are explicit, never inferred.
type Origin struct {
	Version int      `json:"version"`
	Key     string   `json:"key"`               // the directory name under StoreDir(); harnessStoreKey() for the config checkout
	Origin  string   `json:"origin"`            // canonical main-checkout path (EvalSymlinks); "" only for the store root
	Aliases []string `json:"aliases,omitempty"` // other canonical checkout paths accepted as this repo — explicit only
}

// errOriginConflict is returned instead of ever overwriting an origin record that names a different repo.
var errOriginConflict = errors.New("origin record exists with a different canonical origin")

// readOrigin loads and validates dir's origin record; dir must be a real directory directly under StoreDir().
func readOrigin(dir string) (Origin, error) {
	key, err := originKey(dir)
	if err != nil {
		return Origin{}, err
	}
	found, errs, verr := resolveOrigins([]string{key})
	if o, ok := found[key]; ok {
		return o, nil
	}
	if verr != nil && errors.Is(errs[key], fs.ErrNotExist) {
		return Origin{}, fmt.Errorf("%s: origin vault unreadable: %w", dir, verr) // not "no record": callers must not write
	}
	return Origin{}, errs[key]
}

// originKey is dir's vault row key (its store key); symlinks and dirs outside the store have none.
func originKey(dir string) (string, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s: not a directory", dir)
	}
	if !paths.SamePath(filepath.Dir(dir), StoreDir()) {
		return "", fmt.Errorf("%s: not a store directory under %s", dir, StoreDir())
	}
	return filepath.Base(dir), nil
}

// vaultRetryAt defers vault reads after one failed (each waits out View's lock timeout), so a hook pays it once.
var vaultRetryAt atomic.Int64

const vaultRetryDelay = 30 * time.Second

var errVaultSkipped = errors.New("vault read skipped after a recent failure")

// originRows reads every origin row in one View, skipped while a recent read failed.
var originRows = func() (map[string]string, error) {
	if time.Now().UnixNano() < vaultRetryAt.Load() {
		return nil, errVaultSkipped
	}
	var rows map[string]string
	if err := vault.View(func(t *vault.Tx) error {
		rows = t.All(vault.MemoryOrigins)
		return nil
	}); err != nil {
		vaultRetryAt.Store(time.Now().Add(vaultRetryDelay).UnixNano())
		return nil, err
	}
	return rows, nil
}

// resolveOrigins maps store keys to records: vault row, else index.md. Fallback hits become
// rows unless the vault is unreadable, which leaves the call read-only. errs holds the misses.
func resolveOrigins(keys []string) (found map[string]Origin, errs map[string]error, vaultErr error) {
	rows, verr := originRows()
	found, errs = map[string]Origin{}, map[string]error{}
	var catalog map[string]Origin
	var imports []Origin
	for _, key := range keys {
		dir := filepath.Join(StoreDir(), key)
		if raw, ok := rows[key]; ok {
			if o, err := decodeOrigin(dir, []byte(raw)); err != nil {
				errs[key] = err
			} else {
				found[key] = o
			}
			continue
		}
		// Migration: remove after 2026-11-17 once every machine has run a post-2026-10-20 build.
		if catalog == nil {
			catalog = catalogOrigins()
		}
		o, ok := catalog[key]
		if !ok {
			errs[key] = fmt.Errorf("%s: no origin record: %w", dir, fs.ErrNotExist)
			continue
		}
		if err := validateOrigin(dir, o); err != nil {
			errs[key] = err
			continue
		}
		found[key] = o
		imports = append(imports, o)
	}
	if verr == nil && len(imports) > 0 {
		_ = importOrigins(imports) // best effort on a read path: a failed import is retried by the next read
	}
	return found, errs, verr
}

// Migration: remove after 2026-11-17 once every machine has run a post-2026-10-20 build.
// importOrigins writes the rows still absent in one transaction.
func importOrigins(origins []Origin) error {
	return vault.Update(func(t *vault.Tx) error {
		for _, o := range origins {
			if _, ok := t.Get(vault.MemoryOrigins, o.Key); ok {
				continue // another process recorded it first
			}
			data, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if err := t.Put(vault.MemoryOrigins, o.Key, string(data)); err != nil {
				return err
			}
		}
		return nil
	})
}

// decodeOrigin parses and validates one stored record for dir.
func decodeOrigin(dir string, data []byte) (Origin, error) {
	var o Origin
	if err := json.Unmarshal(data, &o); err != nil {
		return Origin{}, fmt.Errorf("%s: origin record: %w", dir, err)
	}
	if err := validateOrigin(dir, o); err != nil {
		return Origin{}, err
	}
	o.Aliases = cleanAliases(o.Aliases)
	return o, nil
}

// validateOrigin rejects records that do not describe dir itself.
func validateOrigin(dir string, o Origin) error {
	switch {
	case o.Version != OriginVersion:
		return fmt.Errorf("%s: origin record version %d, want %d", dir, o.Version, OriginVersion)
	case o.Key != filepath.Base(dir):
		return fmt.Errorf("%s: origin record key %q does not match directory", dir, o.Key)
	case o.Origin == "" && !paths.SamePath(dir, StoreDir()):
		return fmt.Errorf("%s: origin record has an empty origin", dir)
	}
	return nil
}

// cleanAliases cleans, drops empties and dedupes while keeping order.
func cleanAliases(in []string) []string {
	var out []string
	for _, a := range in {
		if a == "" {
			continue
		}
		if a = filepath.Clean(a); !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// WriteOrigin records dir's origin; an existing record for the same Key+Origin is kept as is,
// any other existing record (including an unreadable one) is never overwritten.
func WriteOrigin(dir string, o Origin) error {
	if err := validateOrigin(dir, o); err != nil {
		return err
	}
	cur, err := readOrigin(dir)
	switch {
	case err == nil && cur.Key == o.Key && cur.Origin == o.Origin:
		return nil
	case err == nil:
		return fmt.Errorf("%s: %w", dir, errOriginConflict)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("refusing to overwrite: %w", err)
	}
	return foldOrigin(dir, o, "")
}

// foldOrigin writes dir's record and, when drop is set, deletes that folded store's row in the same transaction.
func foldOrigin(dir string, o Origin, drop string) error {
	key, err := originKey(dir)
	if err != nil {
		return err
	}
	o.Aliases = cleanAliases(o.Aliases)
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return vault.Update(func(t *vault.Tx) error {
		if drop != "" {
			if err := t.Put(vault.MemoryOrigins, drop, ""); err != nil {
				return err
			}
		}
		return t.Put(vault.MemoryOrigins, key, string(data))
	})
}

// catalogOriginTag opens the machine-readable origin comment BuildCatalog appends to each repo row.
const catalogOriginTag = " <!-- origin: "

// catalogOrigin is the comment's JSON payload; json.Marshal escapes '>' so a path can never close the comment.
type catalogOrigin struct {
	Origin  string   `json:"origin"`
	Aliases []string `json:"aliases,omitempty"`
}

// catalogOriginComment is the trailing comment recording o on its catalog row.
func catalogOriginComment(o Origin) string {
	data, _ := json.Marshal(catalogOrigin{Origin: o.Origin, Aliases: o.Aliases})
	return catalogOriginTag + string(data) + " -->"
}

// Migration: remove after 2026-11-17 once every machine has run a post-2026-10-20 build.
// catalogOrigins parses the origin comments of index.md's repo rows ("- <key> — …"), keyed by store key.
func catalogOrigins() map[string]Origin {
	out := map[string]Origin{}
	data, err := os.ReadFile(filepath.Join(StoreDir(), catalogFile))
	if err != nil {
		return out
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, rest, ok := strings.Cut(strings.TrimPrefix(line, "- "), " — ")
		_, after, ok0 := strings.CutLast(rest, catalogOriginTag)
		if !ok || !strings.HasPrefix(line, "- ") || !ok0 || !strings.HasSuffix(rest, " -->") {
			continue
		}
		var c catalogOrigin
		if json.Unmarshal([]byte(strings.TrimSuffix(after, " -->")), &c) == nil {
			out[key] = Origin{Version: OriginVersion, Key: key, Origin: c.Origin, Aliases: cleanAliases(c.Aliases)}
		}
	}
	return out
}
