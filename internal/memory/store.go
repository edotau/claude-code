// Package memory is the session memory bank: six typed markdown files per git repo in one flat
// <ConfigDir>/memory/ store, named <repo>--<file>; the home bank's files are unprefixed.
package memory

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

//go:embed templates/*.md
var templatesFS embed.FS

// fileCaps are the per-file line caps `claude-code memory update` holds; sessionHistory rotates by entry count instead.
var fileCaps = map[string]int{
	"projectContext.md": 60,
	"activeContext.md":  80,
	"progress.md":       60,
	"decisionLog.md":    200,
	"conventions.md":    80,
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
		if err := seedFile(p, body); err != nil {
			return "", err
		}
	}
	return SubDir(""), nil
}

// writeBody writes a new file's body (seedFile, createExcl); tests swap it to fail mid-write.
var writeBody = func(f *os.File, body string) error { _, err := f.WriteString(body); return err }

// seedFile writes body to a temp file and links it into place, so p is never partial and never replaces a racer's file.
func seedFile(p, body string) error {
	f, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // the temp name is spare once linked, garbage if the write failed
	werr := writeBody(f, body)
	if err := errors.Join(werr, f.Chmod(0o600), f.Close()); err != nil {
		return err
	}
	err = os.Link(tmp, p)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	return createExcl(p, body, 0o600) // no hard links here (FUSE, network mounts)
}

// createExcl writes body to a new p, never replacing an existing or racing writer's file.
func createExcl(p, body string, mode os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := errors.Join(writeBody(f, body), f.Close()); err != nil {
		return errors.Join(err, os.Remove(p)) // Init skips an existing file, so a partial one must not survive
	}
	return nil
}

// isProjectContextTemplate reports whether projectContext.md still equals the scaffold, so init offers enrichment.
func isProjectContextTemplate() bool {
	data, err := os.ReadFile(Path("projectContext.md"))
	if err != nil {
		return false
	}
	return string(data) == templates()["projectContext.md"]
}

// templates seed a fresh bank's six files; `claude-code memory update` fills them from real session work.
var templates = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(BankFiles()))
	for _, name := range BankFiles() {
		body, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			panic(err) // embedded alongside bankFiles; a mismatch is a build-time bug, not a runtime one
		}
		m[name] = string(body)
	}
	return m
})

// catalogFile holds every MEMORY.md entry, append-only; routerFile is the generated router BuildCatalog writes.
const catalogFile, routerFile = "index.md", "catalog.md"

// StoreDir is the memory store root and the work tree of the local memory repo.
func StoreDir() string { return filepath.Join(paths.ConfigDir(), "memory") }

// JournalDir holds pending commit-transaction journals; state only, never committed.
func JournalDir() string { return filepath.Join(StateDir(), "journal") }

// AccessStateDir holds the access logs; the single source of truth for their path.
func AccessStateDir() string { return filepath.Join(StateDir(), "access") }

// insideStore reports whether p resolves under the store (and not under its .git); rel is slash-separated.
func insideStore(p string) (rel string, ok bool) {
	store := paths.CanonDeep(StoreDir())
	r, err := filepath.Rel(store, paths.CanonDeep(p))
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	rel = filepath.ToSlash(r)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", false
	}
	return rel, true
}

// StateDir holds update stamps, results, logs and the session chain.
func StateDir() string { return filepath.Join(paths.StateDir(), "memory") }

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
	// Inside the store its own toplevel is the store root; keep cwd so the <repo>-- files resolve to that repo.
	if top != "" && !paths.SamePath(top, StoreDir()) {
		return top
	}
	return cwd
}

// bankFiles are the six typed files a bank carries (order = injection order).
var bankFiles = []string{
	"projectContext.md", "activeContext.md", "progress.md",
	"decisionLog.md", "conventions.md", "sessionHistory.md",
}

// BankFiles exports the six typed filenames for callers outside this package.
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
func BankLockPath() string { return filepath.Join(StateDir(), bankLockFile) }

// storeLockDir is the lock dir (named by stateKey), created 0700.
func storeLockDir() string {
	d := filepath.Join(StateDir(), "locks", stateKey())
	_ = os.MkdirAll(d, 0o700)
	return d
}

// stateKey names b in per-bank state (locks, stamps, notices): always "home".
func stateKey() string { return homeDir }

// WithBankLock runs fn under the bank lock (wait<=0 = BankLockWait); not reentrant, so *Locked helpers never take it.
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

// gitTimeout bounds one git call; a var so tests need not wait it out.
var gitTimeout = 30 * time.Second

// gitOut runs `git -C dir args` with the sanitized env and gitTimeout; for identity probes of OTHER repos only.
var gitOut = func(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	return proc.RunGit(ctx, "", append([]string{"-C", dir}, args...)...)
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
	target := StoreDir()
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
	for _, cwd := range []string{root, paths.ConfigDir(), StoreDir(), home} {
		if cwd != "" {
			targets[paths.ProjectSlug(cwd)] = StoreDir()
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
