// Package memory is the session memory bank: six typed markdown files per git repo under
// <ConfigDir>/docs/memory/bank/<repo-slug>, plus the shared global bank at that tree's top.
package memory

import (
	"os"
	"os/exec"
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

// GlobalDir is the shared bank at the top of the bank tree.
func GlobalDir() string { return filepath.Join(paths.ConfigDir(), "docs", "memory", "bank") }

// Dir is root's local bank; the config dir and any non-repo root collapse to the global one.
func Dir(root string) string {
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
	out, err := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree").Output()
	in := err == nil && strings.TrimSpace(string(out)) == "true"
	worktrees.Store(root, in)
	return in
}

// MigrateLegacy moves a pre-bank/ tree (global files and <slug>/ dirs directly under docs/memory) into
// docs/memory/bank; idempotent, never overwrites, returns the entries moved.
func MigrateLegacy() (int, error) {
	legacy := filepath.Dir(GlobalDir())
	ents, err := os.ReadDir(legacy)
	if err != nil || (len(ents) == 1 && ents[0].Name() == "bank") {
		return 0, nil
	}
	n := 0
	err = paths.WithFileLock(filepath.Join(GlobalDir(), ".migrate"), func() error {
		ents, _ = os.ReadDir(legacy)
		for _, e := range ents {
			if e.Name() == "bank" {
				continue
			}
			to := filepath.Join(GlobalDir(), e.Name())
			if _, err := os.Lstat(to); err == nil {
				continue // destination exists: leave the legacy entry for a human
			}
			if err := os.Rename(filepath.Join(legacy, e.Name()), to); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
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
func Init(root string) (string, error) {
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
