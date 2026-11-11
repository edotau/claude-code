// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/claude-code/), home bank in memory/home/.
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

// bankExists reports whether any typed file exists in root's local bank.
func bankExists(root string) bool {
	dir := Dir(root)
	for _, name := range BankFiles() {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// Init ensures root's bank exists and seeds missing files (never overwrites); returns the bank dir.
func Init(root string) (string, error) {
	dir, err := EnsureRepoDir(root) // a new repo store needs its origin record before any file lands in it
	if err != nil {
		return "", err
	}
	for name, body := range templates() {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := seedFile(p, body); err != nil {
			return "", err
		}
	}
	return dir, nil
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
	return createExcl(p, body) // no hard links here (FUSE, network mounts)
}

// createExcl writes body to a new p, never replacing a racing writer's file.
func createExcl(p, body string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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

// isProjectContextTemplate reports whether root's projectContext.md still equals the scaffold
// template — unfilled, so `memory init` should still offer its enrichment procedure.
func isProjectContextTemplate(root string) bool {
	data, err := os.ReadFile(filepath.Join(Dir(root), "projectContext.md"))
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

// catalogFile is the reserved global catalog at the store root; BuildCatalog is its only writer.
const catalogFile = "index.md"

// storeReservedDirs are store subdirectories that are never a repo's memory dir.
var storeReservedDirs = map[string]bool{
	".git": true, "archive": true, "handoffs": true, "reports": true, "home": true,
}

// StoreDir is the memory store root and the work tree of the local memory repo.
func StoreDir() string { return filepath.Join(paths.ConfigDir(), "memory") }

// harnessStoreDir is the harness's own memory dir inside the store; keyed by the same rule as any repo.
func harnessStoreDir() string { return filepath.Join(StoreDir(), harnessStoreKey()) }

// archiveDir is dir's cold tier for rotated entries and prior revisions.
func archiveDir(dir string) string { return filepath.Join(dir, "archive") }

// HandoffsDir is dir's long-form session handoffs.
func HandoffsDir(dir string) string { return filepath.Join(dir, "handoffs") }

// ReportsDir is dir's command/agent reports (reviews, audits, research briefs).
func ReportsDir(dir string) string { return filepath.Join(dir, "reports") }

// knowledgeDir is dir's local, near-rules tier: durable lessons kept short of a rule.
func knowledgeDir(dir string) string { return filepath.Join(dir, "knowledge") }

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

// HomeBankDir is the shared bank: store/home.
func HomeBankDir() string { return filepath.Join(StoreDir(), "home") }

// Dir is root's local bank; an unresolvable repo gets an inert state-dir path, never the home bank.
func Dir(root string) string {
	if d, err := RepoDir(root); err == nil { // one dir per repo shared with native memory; the harness is memory/claude-code
		return d
	}
	if Resolve(root).Harness { // a vault failure also resolves to the home tier, which Dir must not follow
		return harnessStoreDir()
	}
	return filepath.Join(StateDir(), "unresolved-store")
}

// isHomeBank reports whether root's local bank is the home bank (read it once, not twice).
func isHomeBank(root string) bool { return paths.SamePath(Dir(root), HomeBankDir()) }

// root resolves --repo → $CLAUDE_PROJECT_DIR (only when it is a repo or the config dir) → git toplevel → cwd.
// A launcher may stamp CLAUDE_PROJECT_DIR with a non-repo parent (e.g. $HOME); honoring that attached every
// bank verb to a junk root instead of the repo the user is standing in.
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
	// Inside the store its own toplevel is the store root; keep cwd so memory/<repo>/ resolves to that repo.
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

// BankFiles exports the six typed filenames for callers outside this package (e.g. internal/setup)
// that must watch them without duplicating the list.
func BankFiles() []string { return slices.Clone(bankFiles) }

// sessionHistoryMaxEntries rotates the oldest sessionHistory entries to <bank>/archive/.
const sessionHistoryMaxEntries = 30

// bankLockFile is the ONE store-wide lock; every harness write and every memory Git operation runs under it.
const bankLockFile = "bank.lock"

// BankLockWait bounds how long a worker waits for the bank before giving up with ErrBankBusy.
var BankLockWait = 5 * time.Second

// ErrBankBusy is returned when the bank lock is not granted within the wait.
var ErrBankBusy = errors.New("memory store busy: another harness worker holds the bank lock")

// BankLockPath is the PHYSICAL lock file; nothing appends ".lock" to it (plan §3.1: no .lock.lock variants).
func BankLockPath() string { return filepath.Join(StateDir(), bankLockFile) }

// storeLockDir is state's lock dir for dir's bank (its base name, "home" for the home bank), created 0700.
func storeLockDir(dir string) string {
	key := filepath.Base(dir)
	if paths.SamePath(dir, HomeBankDir()) {
		key = "home"
	}
	d := filepath.Join(StateDir(), "locks", key)
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

// linkResult is one projects/<slug>/memory link the harness maintains (Appendix D: native follows the symlink).
type linkResult struct {
	Link, Target string
	Action       string // linked | ok | relinked | blocked
	Detail       string
}

// projectsMemory is Claude Code's native memory dir for a project slug.
func projectsMemory(slug string) string {
	return filepath.Join(paths.ConfigDir(), "projects", slug, "memory")
}

// linkNativeFor repairs the native link for one session cwd, keyed the way Claude Code names the project.
func linkNativeFor(cwd string) (linkResult, bool) {
	if cwd == "" {
		return linkResult{}, false
	}
	root := filepath.Clean(cwd)
	top := gitMain(root)
	if top != "" {
		root = paths.CanonDeep(top)
	}
	target, err := repoDirWith(cwd, func(string) string { return top }) // RepoDir without a second git probe
	if err != nil {
		return linkResult{}, false
	}
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
	add := func(cwd, target string) {
		if cwd == "" || target == "" {
			return
		}
		slug := paths.ProjectSlug(cwd)
		if prior, ok := targets[slug]; ok && prior != target {
			targets[slug] = ""
			return
		}
		targets[slug] = target
	}
	if target, err := RepoDir(root); root != "" && err == nil {
		add(root, target)
	}
	add(paths.ConfigDir(), harnessStoreDir())
	add(StoreDir(), HomeBankDir())
	if home, err := os.UserHomeDir(); err == nil {
		add(home, HomeBankDir())
	}
	entries, _ := RepoDirs()
	for _, e := range entries {
		add(e.Dir, e.Dir)
		add(e.Origin.Origin, e.Dir)
		for _, alias := range e.Origin.Aliases {
			add(alias, e.Dir)
		}
	}
	return targets
}

// brokenNativeLinks reports only dangling links and links targeting the store root.
func brokenNativeLinks(root string) []linkResult {
	var targets map[string]string // built on the first broken link: it forks git and reads the catalog
	matches, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "memory"))
	var out []linkResult
	for _, link := range matches {
		target := nativeLinkTarget(link)
		if target == "" {
			continue
		}
		_, err := os.Stat(link)
		if !errors.Is(err, fs.ErrNotExist) && !paths.SamePath(target, StoreDir()) {
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
