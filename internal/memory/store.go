// Package memory manages the shared memory bank and its recall state.
package memory

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

// fileCaps are the per-file line caps `claude-code memory update` holds; sessionHistory rotates by entry count instead.
var fileCaps = map[string]int{
	"activeContext.md": 80,
	"progress.md":      60,
	"decisionLog.md":   200,
	"conventions.md":   80,
}

// bankExists reports whether any typed file exists in the bank.
func bankExists() bool {
	for _, name := range BankFiles() {
		if _, err := os.Stat(Path(name)); err == nil {
			return true
		}
	}
	return false
}

// Init ensures the bank exists and seeds missing files (never overwrites); returns the dir holding them.
func Init() (string, error) {
	if err := EnsureBank(""); err != nil {
		return "", err
	}
	for name, body := range templates() {
		p := Path(name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if _, err := paths.WriteNoReplace(p, []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return paths.MemorySubDir(""), nil
}

// isBankTemplate reports whether activeContext.md still equals the scaffold, so init offers enrichment.
func isBankTemplate() bool {
	data, err := os.ReadFile(Path("activeContext.md"))
	return err == nil && string(data) == templates()["activeContext.md"]
}

// templates seed a fresh bank's five files; `claude-code memory update` fills them from real session work.
var templates = sync.OnceValue(func() map[string]string {
	return map[string]string{
		"activeContext.md":  assets.MemoryActiveContextSeed(),
		"conventions.md":    assets.MemoryConventionsSeed(),
		"decisionLog.md":    assets.MemoryDecisionLogSeed(),
		"progress.md":       assets.MemoryProgressSeed(),
		"sessionHistory.md": assets.MemorySessionHistorySeed(),
	}
})

// catalogFile holds every MEMORY.md entry, append-only; routerFile is the generated router BuildCatalog writes.
const catalogFile, routerFile = "index.md", "catalog.md"

// JournalDir holds pending commit-transaction journals; state only, never committed.
func JournalDir() string { return filepath.Join(paths.MemoryStateDir(), "journal") }

// AccessStateDir holds the access logs; the single source of truth for their path.
func AccessStateDir() string { return filepath.Join(paths.MemoryStateDir(), "access") }

func root(repo string) string {
	if repo != "" {
		abs, _ := filepath.Abs(repo)
		return abs
	}
	cwd, _ := os.Getwd()
	top, ran := "", false
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		if paths.SamePath(d, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(d), paths.ConfigDir()) {
			return d
		}
		if top = GitToplevel(d); paths.SamePath(top, d) {
			return d
		}
		ran = d == cwd // the same probe answers for cwd below: run git once
	}
	if !ran {
		top = GitToplevel(cwd)
	}
	// Inside the store, keep cwd rather than its Git toplevel.
	if top != "" && !paths.SamePath(top, paths.MemoryStoreDir()) {
		return top
	}
	return cwd
}

// bankFiles are the five typed files a bank carries (order = injection order).
var bankFiles = []string{
	"activeContext.md", "progress.md", "decisionLog.md", "conventions.md", "sessionHistory.md",
}

// BankFiles exports the five typed filenames for callers outside this package.
func BankFiles() []string { return slices.Clone(bankFiles) }

// sessionHistoryMaxEntries rotates the oldest sessionHistory entries to archive/sessionHistory-<yyyy-mm>.md.
const sessionHistoryMaxEntries = 30

// bankLockFile is the ONE store-wide lock; every harness write and every memory Git operation runs under it.
const bankLockFile = "bank.lock"

// BankLockWait bounds how long a worker waits for the bank before giving up with ErrBankBusy.
var BankLockWait = 5 * time.Second

// ErrBankBusy is returned when the bank lock is not granted within the wait.
var ErrBankBusy = errors.New("memory store busy: another harness worker holds the bank lock")

// BankLockPath is the PHYSICAL lock file; nothing appends ".lock" to it (no .lock.lock variants).
func BankLockPath() string { return filepath.Join(paths.MemoryStateDir(), bankLockFile) }

// storeLockDir is the lock dir (named by stateKey), created 0700.
func storeLockDir() string {
	d := filepath.Join(paths.MemoryStateDir(), "locks", stateKey())
	_ = os.MkdirAll(d, 0o700)
	return d
}

// stateKey names b in per-bank state (locks, stamps, notices): always "home".
func stateKey() string { return homeDir }

// WithBankLock runs fn under the bank lock (wait<=0 = BankLockWait); not reentrant, so *Locked helpers never take it.
func WithBankLock(wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
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

// gitTimeout bounds one git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// gitOut runs `git -C dir args` with the sanitized env and gitTimeout; for identity probes of OTHER repos only.
var gitOut = func(dir string, args ...string) (string, error) {
	return proc.GitTimeout(gitTimeout, dir, args...)
}

// GitToplevel is dir's git work-tree root, "" outside a repo.
func GitToplevel(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// linkResult is one projects/<slug>/memory link the harness maintains (native auto-memory follows the symlink).
type linkResult struct {
	Link, Target string
	Action       string // linked | ok | relinked | blocked
	Detail       string
}

// projectsMemory is Claude Code's native memory dir for a project slug.
func projectsMemory(slug string) string {
	return filepath.Join(paths.ConfigDir(), "projects", slug, "memory")
}

// nativeRoots remembers the slug root linkNativeFor resolved per cleaned cwd, so nativeLinked skips a second git fork.
var nativeRoots sync.Map

// linkNativeFor repairs the native link for one session cwd, keyed the way Claude Code names the project.
func linkNativeFor(cwd string) (linkResult, bool) {
	if cwd == "" {
		return linkResult{}, false
	}
	root := paths.CanonDeep(filepath.Clean(cwd)) // Claude Code names the project by the resolved path (/tmp → /private/tmp)
	top, _ := gitMain(root)
	if top != "" {
		root = paths.CanonDeep(top)
	}
	nativeRoots.Store(filepath.Clean(cwd), root)
	target := paths.MemoryStoreDir()
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return linkResult{}, false // no store yet: never mint an origin-less dir that would squat the repo's key
	}
	repairNativeLinks(cwd)
	return ensureLink(projectsMemory(paths.ProjectSlug(root)), target), true
}

// ensureLink makes link a symlink to target: absent or empty dir → linked, wrong link → relinked, content → blocked.
func ensureLink(link, target string) linkResult {
	r := linkResult{Link: link, Target: target}
	fi, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.Action = "linked"
	case err != nil:
		return r.blocked(err.Error())
	case fi.Mode()&fs.ModeSymlink != 0:
		return relink(r)
	case fi.IsDir():
		if r = clearEmptyDir(r); r.Action != "linked" {
			return r
		}
	default:
		return r.blocked("not a directory")
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return r.blocked(err.Error())
	}
	if err := os.Symlink(target, link); err != nil {
		r.Action, r.Detail = "blocked", err.Error()
	}
	return r
}

// blocked returns r marked blocked with detail.
func (r linkResult) blocked(detail string) linkResult {
	r.Action, r.Detail = "blocked", detail
	return r
}

// relink swaps a wrong symlink atomically (Action "relinked"), keeps a right one (Action "ok"), or reports blocked.
func relink(r linkResult) linkResult {
	if paths.SamePath(nativeLinkTarget(r.Link), r.Target) {
		r.Action = "ok"
		return r
	}
	cur, _ := os.Readlink(r.Link)
	if err := paths.SwapSymlink(r.Target, r.Link); err != nil {
		return r.blocked(err.Error())
	}
	r.Action, r.Detail = "relinked", "was -> "+cur
	return r
}

// clearEmptyDir removes an empty real dir (Action "linked"); a dir with content is blocked.
func clearEmptyDir(r linkResult) linkResult {
	if ents, err := os.ReadDir(r.Link); err != nil || len(ents) > 0 {
		return r.blocked("real native dir with content: merge it into the store dir by hand")
	}
	if err := os.Remove(r.Link); err != nil {
		return r.blocked(err.Error())
	}
	r.Action = "linked"
	return r
}

// nativeLinkTarget resolves relative symlink text even when its destination no longer exists.
func nativeLinkTarget(link string) string {
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return paths.CanonDeep(target)
}

// nativeLinkTargets uses recorded paths because project slugs cannot be decoded unambiguously.
func nativeLinkTargets(root string) map[string]string {
	targets := map[string]string{}
	home, _ := os.UserHomeDir()
	for _, cwd := range []string{root, paths.ConfigDir(), paths.MemoryStoreDir(), home} {
		if cwd != "" {
			targets[paths.ProjectSlug(cwd)] = paths.MemoryStoreDir()
		}
	}
	return targets
}

// brokenNativeLinks reports only dangling links and links targeting the store root.
func brokenNativeLinks(root string) []linkResult {
	var targets map[string]string // built on the first broken link: it forks git and reads the catalog
	var out []linkResult
	for _, link := range transcript.GlobIn(filepath.Join(paths.ConfigDir(), "projects"), "*/memory") {
		target := nativeLinkTarget(link)
		if target == "" {
			continue
		}
		_, err := os.Stat(link)
		if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if targets == nil {
			targets = nativeLinkTargets(root)
		}
		r := linkResult{Link: link, Action: "blocked", Detail: "cwd cannot be derived from recorded paths"}
		if r.Target = targets[filepath.Base(filepath.Dir(link))]; r.Target != "" {
			if fi, statErr := os.Stat(r.Target); statErr != nil || !fi.IsDir() {
				r.Detail = "resolved store directory is unavailable"
			} else {
				r.Action, r.Detail = "repairable", "invalid native memory target: "+target
			}
		}
		out = append(out, r)
	}
	return out
}

// repairNativeLinks leaves unresolved slugs alone for memory review to report.
func repairNativeLinks(root string) {
	for _, r := range brokenNativeLinks(root) {
		if r.Action == "repairable" {
			ensureLink(r.Link, r.Target)
		}
	}
}

func nativeLinkProblems(root string) []string {
	var problems []string
	for _, r := range brokenNativeLinks(root) {
		problems = append(problems, "native memory link "+r.Link+": "+r.Detail)
	}
	return problems
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// resolve maps root to its canonical identity; any git failure is the home tier, never an error.
func resolve(root string) string { return resolveWith(root, gitMain) }

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

// resolveWith is resolve with main supplying root's git main checkout on a memo miss.
func resolveWith(root string, main func(string) (string, bool)) string {
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
			return ""
		}
		origin = o
		identities.Store(root, origin)
	}
	o := origin.(string)
	switch {
	case o == "":
		return ""
	case paths.SamePath(o, paths.ConfigDir()):
		return harnessIdentity()
	}
	return o
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
func storeIdentity(root string) (string, bool) {
	if !paths.Within(paths.CanonDeep(root), paths.CanonDeep(paths.MemoryStoreDir())) {
		return "", false
	}
	return "", true
}

func harnessIdentity() string {
	return paths.CanonDeep(paths.ConfigDir())
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

// throwawayOff keeps temporary test repositories eligible as projects.
var throwawayOff = testing.Testing()

// throwaway reports whether root sits in a scratch or fixture folder, which must never mint a store.
func throwaway(root string) bool {
	if throwawayOff {
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
func fixedIdentity(root string) (string, bool) {
	if id, ok := storeIdentity(root); ok {
		return id, true
	}
	if throwaway(root) {
		return throwawayIdentity(), true
	}
	return "", false
}

// throwawayIdentity is the launching workspace from OriginEnv, else the home tier.
func throwawayIdentity() string {
	o := os.Getenv(OriginEnv)
	if o == "" || !filepath.IsAbs(o) {
		return ""
	}
	if paths.SamePath(o, paths.ConfigDir()) {
		return harnessIdentity()
	}
	o = paths.CanonDeep(o)
	return o
}

// LaunchOrigin is the origin a child launched from cwd inherits: OriginEnv wins; the home tier passes nothing.
func LaunchOrigin(cwd string) string {
	if o := os.Getenv(OriginEnv); o != "" {
		return o
	}
	return resolve(cwd)
}

const dateLayout = "2006-01-02"

var (
	repoKeyRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*--`) })
	dateRE    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\d{4}-\d{2}-\d{2}`) })
	compactRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})-`) })
)

func validDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}

// StripRepoPrefix drops every leading `<key>--` and returns the first key; a name with none comes back whole.
func StripRepoPrefix(name string) (rest, key string) {
	rest = name
	for {
		k := repoKeyRE().FindString(rest)
		if k == "" || len(k) == len(rest) {
			return rest, key
		}
		if key == "" {
			key = k[:len(k)-2]
		}
		rest = rest[len(k):]
	}
}

// DatedName strips the repo prefix and leads the name with a date: its own, one inside it, else now.
func DatedName(name string, now time.Time) string {
	rest, _ := StripRepoPrefix(name)
	if len(rest) > 10 && rest[10] == '-' && validDate(rest[:10]) {
		return rest
	}
	if m := compactRE().FindStringSubmatch(rest); m != nil && validDate(m[1]+"-"+m[2]+"-"+m[3]) {
		return m[1] + "-" + m[2] + "-" + m[3] + rest[8:]
	}
	for _, d := range dateRE().FindAllString(rest, -1) {
		if validDate(d) {
			return d + "-" + rest
		}
	}
	return now.Format(dateLayout) + "-" + rest
}
