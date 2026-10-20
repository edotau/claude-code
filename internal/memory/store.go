// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/memory/<repo>/ (harness itself memory/.claude/), global at the top.
package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

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

// FileCaps are the per-file line caps /memory:end holds; sessionHistory rotates by entry count instead.
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

// GlobalDir is the shared bank: the store root after the cutover, else the top of the legacy bank tree.
func GlobalDir() string {
	if CutOver() {
		return StoreDir()
	}
	return LegacyGlobalDir()
}

// Dir is root's local bank; the config dir and any non-repo root collapse to the global one.
func Dir(root string) string {
	if CutOver() { // one dir per repo shared with native memory; the harness is memory/.claude
		if d, err := RepoDir(root); err == nil {
			return d
		}
		return StoreDir()
	}
	if slug := paths.RepoSlug(root); slug != "" && inWorkTree(root) {
		return filepath.Join(GlobalDir(), slug)
	}
	return GlobalDir()
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

// IsGlobal reports whether root's local bank is the global bank (read it once, not twice).
func IsGlobal(root string) bool { return paths.SamePath(Dir(root), GlobalDir()) }

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
	if CutOver() { // a new repo store needs its .origin before any file lands in it
		d, err := EnsureRepoDir(root)
		if err != nil {
			return "", err
		}
		dir = d
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, body := range templates {
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

// templates seed a fresh bank's six files; /memory:end fills them from real session work.
var templates = map[string]string{
	"projectContext.md": "# Project Context\n\n" +
		"> Mostly-static orientation. Points at canonical docs — never duplicates them.\n\n" +
		"(Describe the project in a line or two; link CLAUDE.md/README.)\n",
	"activeContext.md": "# Active Context\n\n" +
		"> Current session state — auto-injected at session start. REPLACE each `/memory:end`.\n\n" +
		"## Current Focus\n\n## Open Questions\n\n## Blockers\n\n## Next Steps\n\n## Parked\n",
	"progress.md": "# Progress\n\n" +
		"> Task tracking — auto-injected at session start. REPLACE each `/memory:end`.\n\n" +
		"## What Works\n\n## In Progress\n\n## What's Next\n\n## Known Issues\n",
	"decisionLog.md": "# Decision Log\n\n" +
		"> Dated decisions + rationale. APPEND-ONLY, newest first. Supersede, never delete.\n",
	"conventions.md": "# Conventions\n\n" +
		"> Learned patterns/preferences. APPEND one imperative bullet each; prune what no longer holds.\n",
	"sessionHistory.md": "# Session History\n\n" +
		"> Newest-first session summaries. APPEND at top, cap 30; overflow → archive/sessionHistory-<yyyy-mm>.md.\n",
}
