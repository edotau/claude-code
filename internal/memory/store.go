// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/claude-code/), home bank in memory/home/.
package memory

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

//go:embed templates/*.md
var templatesFS embed.FS

// bankFiles are the six typed files a bank carries (order = injection order).
var bankFiles = []string{
	"projectContext.md", "activeContext.md", "progress.md",
	"decisionLog.md", "conventions.md", "sessionHistory.md",
}

// BankFiles exports the six typed filenames for callers outside this package (e.g. internal/history)
// that must watch them without duplicating the list.
func BankFiles() []string { return bankFiles }

// SessionHistoryMaxEntries rotates the oldest sessionHistory entries to <bank>/archive/.
const SessionHistoryMaxEntries = 30

// FileCaps are the per-file line caps /reflection:update holds; sessionHistory rotates by entry count instead.
var FileCaps = map[string]int{
	"projectContext.md": 60,
	"activeContext.md":  80,
	"progress.md":       60,
	"decisionLog.md":    200,
	"conventions.md":    80,
}

// HarvestChildEnv marks a spawned harvest worker so its own session hooks never schedule another.
const HarvestChildEnv = "CLAUDE_SESSION_HARVEST_CHILD"

// IsHarvestChild reports whether this process runs inside a harvest worker.
func IsHarvestChild() bool {
	v := os.Getenv(HarvestChildEnv)
	return v != "" && v != "0"
}

// LegacyGlobalDir is the pre-cutover bank tree; only migration reads it once the cutover lands.
func LegacyGlobalDir() string { return filepath.Join(paths.ConfigDir(), "docs", "memory", "bank") }

// CutOver reports a completed migration: from then on every bank and native path resolves inside StoreDir().
func CutOver() bool {
	_, _, incomplete, migrated := scanJournals()
	return migrated && !incomplete
}

// HomeBankDir is the shared bank: store/home after the cutover, else the top of the legacy bank tree.
func HomeBankDir() string {
	if CutOver() {
		return homeStoreDir()
	}
	return LegacyGlobalDir()
}

// homeStoreDir is the post-cutover home dir, unconditionally: migrate plans the FUTURE layout while
// CutOver() still reports false, so its destinations must never gate on it (HomeBankDir() would self-reference).
// Every caller resolves through here, so the one-time global/ → home/ rename rides along for free.
func homeStoreDir() string {
	ensureHomeBankRenamed()
	return filepath.Join(StoreDir(), "home")
}

// homeBankRenamed memoizes the rename check per store dir (not a single global once: tests run many
// distinct stores, via distinct ConfigDir, in one process).
var homeBankRenamed sync.Map

// ensureHomeBankRenamed renames a leftover store/global/ dir (a store that completed the unified-store
// cutover before this rename landed) to store/home/, once per store dir. Idempotent: a no-op once
// home/ exists or global/ is absent; both present is left alone for `memory review` to flag.
func ensureHomeBankRenamed() {
	store := StoreDir()
	if _, done := homeBankRenamed.LoadOrStore(store, true); done {
		return
	}
	oldDir, newDir := filepath.Join(store, "global"), filepath.Join(store, "home")
	if _, err := os.Stat(newDir); err == nil {
		return
	}
	if _, err := os.Stat(oldDir); err != nil {
		return
	}
	_ = os.Rename(oldDir, newDir)
}

// Dir is root's local bank; a non-repo root collapses to the home one.
func Dir(root string) string {
	if CutOver() { // one dir per repo shared with native memory; the harness is memory/claude-code
		if d, err := RepoDir(root); err == nil {
			return d
		}
		return homeStoreDir()
	}
	if slug := paths.RepoSlug(root); slug != "" && inWorkTree(root) {
		return filepath.Join(HomeBankDir(), slug)
	}
	return HomeBankDir()
}

// worktrees memoizes the git fork: Dir runs several times per hook, and a flip mid-process would split a bank.
var worktrees sync.Map

// inWorkTree is stat-first on .git (a toplevel never forks git), else `git rev-parse --is-inside-work-tree`.
func inWorkTree(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return true
	}
	if v, ok := worktrees.Load(root); ok {
		return v.(bool)
	}
	out, err := gitOut(root, "rev-parse", "--is-inside-work-tree")
	in := err == nil && strings.TrimSpace(out) == "true"
	worktrees.Store(root, in)
	return in
}

// IsHomeBank reports whether root's local bank is the home bank (read it once, not twice).
func IsHomeBank(root string) bool { return paths.SamePath(Dir(root), HomeBankDir()) }

// HasBank reports whether any typed file exists in root's local bank.
func HasBank(root string) bool {
	for _, name := range bankFiles {
		if _, err := os.Stat(filepath.Join(Dir(root), name)); err == nil {
			return true
		}
	}
	return false
}

// Init ensures root's bank exists and seeds missing files (never overwrites); returns the bank dir.
// It is a scaffold write, so the startup guard gates it: a retired legacy bank is never recreated.
func Init(root string) (string, error) {
	if err := CheckWritable(); err != nil {
		return "", err
	}
	dir := Dir(root)
	if CutOver() { // a new repo store needs its origin record before any file lands in it
		d, err := EnsureRepoDir(root)
		if err != nil {
			return "", err
		}
		dir = d
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, body := range templates() {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := paths.AtomicWrite(p, []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// GitToplevel is dir's git work-tree root, "" outside a repo.
func GitToplevel(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// templates seed a fresh bank's six files; /reflection:update fills them from real session work.
var templates = sync.OnceValue(func() map[string]string {
	m := make(map[string]string, len(bankFiles))
	for _, name := range bankFiles {
		body, err := templatesFS.ReadFile("templates/" + name)
		if err != nil {
			panic(err) // embedded alongside bankFiles; a mismatch is a build-time bug, not a runtime one
		}
		m[name] = string(body)
	}
	return m
})
