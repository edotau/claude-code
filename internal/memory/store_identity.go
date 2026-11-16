package memory

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// Identity is a cwd resolved to the canonical repository it belongs to; worktrees and subdirs share one.
type Identity struct {
	Origin   string // canonical main checkout ("" when HomeBank)
	Base     string // filepath.Base(Origin)
	HomeBank bool   // non-repo cwd → unprefixed files in the store (home tier)
	Harness  bool   // the config checkout itself → Key harnessStoreKey()
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// Resolve maps root to its canonical identity; any git failure is the home tier, never an error.
func Resolve(root string) Identity { return resolveWith(root, gitMain) }

// gitMain is root's main checkout per git, "" outside a repo; definitive is false when git itself failed.
func gitMain(root string) (top string, definitive bool) {
	definitive = true
	top = paths.MainCheckout(root, func(dir string, args ...string) (string, error) {
		out, err := gitOut(dir, args...)
		if err != nil && !gitRefused(err) {
			definitive = false
		}
		return out, err
	})
	return top, definitive
}

// gitRefused reports whether err is git running and exiting nonzero (a non-repo, no remote), not failing to run.
func gitRefused(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() > 0
}

// resolveWith is Resolve with main supplying root's git main checkout on a memo miss.
func resolveWith(root string, main func(string) (string, bool)) Identity {
	root = filepath.Clean(root)
	if paths.SamePath(root, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(root), paths.ConfigDir()) {
		return harnessIdentity()
	}
	origin, ok := identities.Load(root)
	if !ok {
		if id, fixed := fixedIdentity(root); fixed {
			return id
		}
		o, memoize := resolveOrigin(root, main)
		if !memoize {
			return Identity{HomeBank: true}
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

// resolveOrigin finds root's origin; memoize is false when git failed to run, so a later call re-asks.
func resolveOrigin(root string, main func(string) (string, bool)) (origin string, memoize bool) {
	o, definitive := main(root)
	if o != "" {
		o = paths.CanonDeep(o)
	}
	return o, definitive || o != ""
}

// storeIdentity maps a cwd inside the store to the home tier, else ok=false.
func storeIdentity(root string) (Identity, bool) {
	if !paths.Within(paths.CanonDeep(root), paths.CanonDeep(StoreDir())) {
		return Identity{}, false
	}
	return Identity{HomeBank: true}, true
}

func harnessIdentity() Identity {
	o := paths.CanonDeep(paths.ConfigDir())
	return Identity{Origin: o, Base: filepath.Base(o), Harness: true}
}

// harnessStoreKey is paths.ConfigDir()'s store key: the origin-remote rule of any repo, else its basename.
func harnessStoreKey() string {
	cfg := paths.ConfigDir()
	if rb := repoKeyFromRemote(originRemoteURL(cfg)); rb != "" {
		return rb
	}
	return filepath.Base(cfg)
}

// remoteCache memoizes originRemoteURL per checkout, misses included; a remote change mid-process is not seen.
var remoteCache sync.Map

// remoteStamp fingerprints the config files that decide `origin`; "" when not cacheable (env overrides or any include).
func remoteStamp(checkout string) string {
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_CONFIG") || strings.HasPrefix(e, "GIT_DIR") {
			return ""
		}
	}
	home, _ := os.UserHomeDir()
	xdg := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	cfgs := []string{
		filepath.Join(checkout, ".git", "config"), filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config"),
		"/etc/gitconfig",
	}
	var b strings.Builder
	for i, p := range cfgs {
		fi, err := os.Stat(p)
		if i == 0 && (err != nil || !fi.Mode().IsRegular()) {
			return ""
		}
		if err != nil {
			continue
		}
		if data, err := os.ReadFile(p); err != nil || hasInclude(data) {
			return "" // an included file is outside the stamp, so git must answer
		}
		fmt.Fprintf(&b, "%d,%d;", fi.ModTime().UnixNano(), fi.Size())
	}
	return b.String()
}

// hasInclude reports whether a git config has an [include] or [includeIf] section header.
func hasInclude(cfg []byte) bool {
	for _, line := range strings.Split(string(cfg), "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "[include") {
			return true
		}
	}
	return false
}

// originRemoteURL is checkout's configured `origin` remote, "" when there is none (or checkout is not a repo).
func originRemoteURL(checkout string) string {
	if v, ok := remoteCache.Load(checkout); ok {
		return v.(string)
	}
	stamp := remoteStamp(checkout)
	file := filepath.Join(StateDir(), "remote", fmt.Sprintf("%x", sha256.Sum256([]byte(checkout)))[:16])
	if data, err := os.ReadFile(file); err == nil && stamp != "" {
		if s, url, ok := strings.Cut(string(data), "\n"); ok && s == stamp {
			remoteCache.Store(checkout, url)
			return url
		}
	}
	out, err := gitOut(checkout, "remote", "get-url", "origin")
	if err != nil {
		if !gitRefused(err) {
			return "" // git failed to run: do not memoize it as "no remote"
		}
		out = ""
	}
	remoteCache.Store(checkout, out)
	if stamp != "" {
		_ = paths.AtomicWrite(file, []byte(stamp+"\n"+out), 0o600) // a cache: a failed write only costs the next fork
	}
	return out
}

// repoKeyFromRemote is a git remote URL's basename without ".git" (ssh, scp-style, https); "" if unparseable.
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

// routeMatch reports whether query names raw as a whole word, with or without its leading dot.
func routeMatch(query, raw string) bool {
	name := strings.ToLower(strings.TrimPrefix(raw, "."))
	if len(name) < 4 {
		return false
	}
	return routeWord(query, strings.ToLower(raw)) || routeWord(query, name)
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
